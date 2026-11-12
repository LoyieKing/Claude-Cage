package workbench

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
)

var errTerminalInputFull = errors.New("terminal input queue is full")

var (
	errTerminalDetached = errors.New("terminal detached")
	errTerminalLost     = errors.New("terminal lost")
	errTerminalGap      = errors.New("terminal output gap")
	errInputUnknown     = errors.New("terminal input unknown")
)

type terminalGap struct {
	From, To uint64
	Omitted  bool
}

type terminalInput struct {
	ID, Data string
}

// terminalFrame is the ordered viewer stream. Replay output and input
// dispositions stay in this sequence; replay is not a side channel.
type terminalFrame struct {
	Data        []byte
	Replay      bool
	InputID     string
	InputStatus string
}

type terminalPTY interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
	Wait() error
	Kill() error
}

type terminalSession struct {
	mu                    sync.Mutex
	id, title             string
	sharedID              string
	connectionID          string
	conversationEnded     bool
	workspace             string
	kind, claudeAccountID string
	loginGuestID          string
	loginLinkMu           sync.Mutex
	loginLinkCleanup      sync.Once
	claudeAccountSource   string
	cmd                   *exec.Cmd
	file                  terminalPTY
	history               []byte
	closed                bool
	clients               map[chan terminalFrame]bool
	gapClients            map[chan terminalGap]bool
	done                  chan struct{}
	inputs                chan terminalInput
	stopping              chan struct{}
	detached              chan struct{}
	detachOnce            sync.Once
	closeOnce             sync.Once
	cliStartupDone        chan struct{}
	state                 string
	histEnd               uint64
	gapFrom, gapTo        uint64
	replayEnd             uint64
}

func (t *terminalSession) ownsConversation() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.closed && !t.conversationEnded
}

func newTerminalSession(file terminalPTY) *terminalSession {
	return &terminalSession{
		file: file, clients: map[chan terminalFrame]bool{}, gapClients: map[chan terminalGap]bool{},
		done: make(chan struct{}), inputs: make(chan terminalInput, 8), stopping: make(chan struct{}),
		detached: make(chan struct{}), state: "running",
	}
}

func (t *terminalSession) summary() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	closed := t.closed
	if t.done != nil {
		select {
		case <-t.done:
			closed = true
		default:
			closed = false
		}
	}
	state := t.summaryStateLocked(closed)
	return map[string]any{"id": t.id, "title": t.title, "closed": closed, "workspace": t.workspace, "kind": t.kind,
		"claude_account_id": t.claudeAccountID, "claude_account_source": t.claudeAccountSource, "cli_session_id": t.sharedID,
		"cli_starting": t.cliStartupPendingLocked(), "state": state, "output_gap": t.gapTo > t.gapFrom}
}

func (t *terminalSession) summaryStateLocked(closed bool) string {
	state := t.state
	if state == "" {
		state = "running"
	}
	if closed && (state == "running" || state == "starting") {
		return "stopped"
	}
	return state
}

func (t *terminalSession) setState(state string) {
	t.mu.Lock()
	t.state = state
	t.mu.Unlock()
}

func (t *terminalSession) skipFinished() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state == "detached" || t.state == "lost"
}

func (t *terminalSession) beginDetach() {
	if t == nil || t.file == nil {
		return
	}
	if marker, ok := t.file.(interface{ beginDetach() }); ok {
		marker.beginDetach()
	}
}

func (t *terminalSession) persistsInGuest() bool {
	_, ok := t.file.(interface{ beginDetach() })
	return ok
}

func (t *terminalSession) signalDetach() {
	t.detachOnce.Do(func() {
		close(t.detached)
		if stopper, ok := t.file.(interface{ detach() }); ok {
			stopper.detach()
		}
	})
}

func (t *terminalSession) cliStartupPendingLocked() bool {
	if t.cliStartupDone == nil || t.closed {
		return false
	}
	select {
	case <-t.cliStartupDone:
		return false
	default:
		return true
	}
}

func (t *terminalSession) finishCLIStartup() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cliStartupPendingLocked() {
		close(t.cliStartupDone)
	}
}

func (t *terminalSession) stopPTY() {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()
		if t.stopping != nil {
			close(t.stopping)
		}
		if t.file != nil {
			// Close may wait inside ConPTY. Never hold the session mutex while
			// the reader needs it to drain the final output and finish.
			_ = t.file.Kill()
			_ = t.file.Close()
		}
	})
}

func (t *terminalSession) close() {
	t.stopPTY()
	if t.done != nil {
		<-t.done
	}
}

// Input belongs to the session once admitted. A viewer never waits inside a
// PTY write, so disconnecting or locking the account can drain its handler
// while a guest process is not reading input. A full queue rejects the viewer
// immediately, so the handler never waits to read a WebSocket close either.
func (t *terminalSession) enqueueInput(ctx context.Context, input string) error {
	return t.enqueueInputID(ctx, "", input)
}

func (t *terminalSession) enqueueInputID(ctx context.Context, id, input string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "" {
		id = randomToken()[:32]
	} else if !validTerminalInputID(id) {
		return errors.New("终端输入编号无效")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return os.ErrClosed
	}
	select {
	case t.inputs <- terminalInput{ID: id, Data: input}:
		t.broadcastLocked(terminalFrame{InputID: id, InputStatus: "accepted"})
		return nil
	default:
		return errTerminalInputFull
	}
}

func validTerminalInputID(id string) bool {
	if len(id) < 16 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if c < '0' || (c > '9' && c < 'a') || c > 'f' {
			return false
		}
	}
	return true
}

func (t *terminalSession) writeInput() {
	for {
		select {
		case <-t.stopping:
			return
		case <-t.detached:
			return
		case input := <-t.inputs:
			_, err := io.WriteString(t.file, input.Data)
			switch {
			case err == nil:
				t.publishInput(input.ID, "written")
			case errors.Is(err, errInputUnknown):
				t.publishInput(input.ID, "unknown")
			case errors.Is(err, errTerminalDetached):
				t.publishInput(input.ID, "unknown")
				return
			default:
				t.publishInput(input.ID, "unknown")
				t.stopPTY()
				return
			}
		}
	}
}

type terminalSubscription struct {
	History     []byte
	Output      <-chan terminalFrame
	InitialGap  *terminalGap
	Gaps        <-chan terminalGap
	Lost        bool
	CLIStartup  <-chan struct{}
	CLIStarting bool
	session     *terminalSession
	channel     chan terminalFrame
	gapCh       chan terminalGap
}

// subscribe captures history and registers live output under the same lock.
// Every byte belongs to exactly one side of this history/live boundary.
func (t *terminalSession) subscribe() *terminalSubscription {
	t.mu.Lock()
	defer t.mu.Unlock()
	channel := make(chan terminalFrame, 64)
	if t.clients == nil {
		t.clients = map[chan terminalFrame]bool{}
	}
	if t.closed {
		close(channel)
	} else {
		t.clients[channel] = true
	}
	gapCh := make(chan terminalGap, 4)
	if t.gapClients == nil {
		t.gapClients = map[chan terminalGap]bool{}
	}
	if t.closed {
		close(gapCh)
	} else {
		t.gapClients[gapCh] = true
	}
	var initial *terminalGap
	if t.kind != "claude-login" && t.gapTo > t.gapFrom {
		gap := terminalGap{From: t.gapFrom, To: t.gapTo}
		initial = &gap
	}
	history := append([]byte(nil), t.history...)
	if t.kind == "claude-login" {
		history = []byte("请在 Cage 浏览器的临时登录页完成授权。若页面提供验证码，可返回此终端输入。\r\n")
	}
	return &terminalSubscription{History: history, Output: channel, InitialGap: initial, Gaps: gapCh, gapCh: gapCh,
		Lost: t.state == "lost", CLIStartup: t.cliStartupDone, CLIStarting: t.cliStartupPendingLocked(), session: t, channel: channel}
}

// Close detaches the viewer; the PTY and its process continue running.
func (s *terminalSubscription) Close() {
	s.session.mu.Lock()
	defer s.session.mu.Unlock()
	if s.session.clients[s.channel] {
		delete(s.session.clients, s.channel)
		close(s.channel)
	}
	if s.gapCh != nil && s.session.gapClients[s.gapCh] {
		delete(s.session.gapClients, s.gapCh)
		close(s.gapCh)
	}
}

func (t *terminalSession) run() {
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); t.writeInput() }()
	err := t.read()
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	switch {
	case errors.Is(err, errTerminalDetached):
		// Detach mode was set before the transport closed. Do not Stop the guest.
		t.signalDetach()
		t.setState("detached")
	case errors.Is(err, errTerminalLost):
		t.setState("lost")
		// Retire the writer without Kill. Close only drops the local attachment.
		t.signalDetach()
		if t.file != nil {
			_ = t.file.Close()
		}
	default:
		// An SSH process can remain alive after its output side closes. Once no
		// output can be read, terminate and reap it before completing the session.
		t.stopPTY()
		if t.file != nil {
			_ = t.file.Wait()
		}
	}
	<-writerDone
	t.mu.Lock()
	pending := make([]terminalInput, 0, len(t.inputs))
	for len(t.inputs) > 0 {
		pending = append(pending, <-t.inputs)
	}
	t.mu.Unlock()
	for _, input := range pending {
		t.publishInput(input.ID, "unknown")
	}
	t.mu.Lock()
	for channel := range t.clients {
		close(channel)
		delete(t.clients, channel)
	}
	for channel := range t.gapClients {
		close(channel)
		delete(t.gapClients, channel)
	}
	if t.state == "" || t.state == "running" || t.state == "starting" {
		t.state = "stopped"
	}
	t.closed = true
	t.mu.Unlock()
	close(t.done)
}

func (t *terminalSession) read() error {
	buffer := make([]byte, 16<<10)
	for {
		n, err := t.file.Read(buffer)
		if errors.Is(err, errTerminalGap) {
			if source, ok := t.file.(interface{ takeGap() (terminalGap, bool) }); ok {
				if gap, ok := source.takeGap(); ok {
					t.publishGap(gap)
				}
			}
			if n > 0 {
				t.deliver(append([]byte(nil), buffer[:n]...), t.readWasReplay())
			}
			continue
		}
		if n > 0 {
			t.deliver(append([]byte(nil), buffer[:n]...), t.readWasReplay())
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (t *terminalSession) readWasReplay() bool {
	marker, ok := t.file.(interface{ lastReadReplay() bool })
	return ok && marker.lastReadReplay()
}

func (t *terminalSession) automationText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return cliStartupText(t.liveHistoryLocked())
}

func (t *terminalSession) liveHistoryLocked() []byte {
	if t.replayEnd == 0 || len(t.history) == 0 {
		return t.history
	}
	start := t.histEnd - uint64(len(t.history))
	if t.replayEnd <= start {
		return t.history
	}
	if t.replayEnd >= t.histEnd {
		return nil
	}
	return t.history[int(t.replayEnd-start):]
}

func (t *terminalSession) publishInput(id, status string) {
	if id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.broadcastLocked(terminalFrame{InputID: id, InputStatus: status})
}

func (t *terminalSession) broadcastLocked(frame terminalFrame) {
	for channel := range t.clients {
		select {
		case channel <- frame:
		default:
			if frame.InputStatus != "" && len(frame.Data) == 0 {
				continue
			}
			close(channel)
			delete(t.clients, channel)
		}
	}
}

func (t *terminalSession) deliver(chunk []byte, replay bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.kind != "claude-login" {
		t.history = append(t.history, chunk...)
		t.histEnd += uint64(len(chunk))
		if replay {
			t.replayEnd = t.histEnd
		}
		const limit = 512 << 10
		if len(t.history) > limit {
			drop := uint64(len(t.history) - limit)
			start := t.histEnd - uint64(len(t.history))
			t.noteGapLocked(start, start+drop)
		}
	}
	t.broadcastLocked(terminalFrame{Data: chunk, Replay: replay})
}

func (t *terminalSession) publishGap(gap terminalGap) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if gap.Omitted {
		for channel := range t.gapClients {
			select {
			case channel <- gap:
			default:
			}
		}
		return
	}
	if t.kind == "claude-login" {
		return
	}
	t.noteGapLocked(gap.From, gap.To)
}

func (t *terminalSession) noteGapLocked(from, to uint64) {
	if to < from || t.kind == "claude-login" {
		return
	}
	start := t.histEnd - uint64(len(t.history))
	if to > start {
		drop := to - start
		if drop >= uint64(len(t.history)) {
			t.history = nil
		} else {
			t.history = append([]byte(nil), t.history[int(drop):]...)
		}
	}
	if to > t.gapTo {
		t.gapFrom, t.gapTo = from, to
	}
	for channel := range t.gapClients {
		select {
		case channel <- terminalGap{From: from, To: to}:
		default:
		}
	}
}
