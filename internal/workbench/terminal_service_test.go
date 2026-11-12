package workbench

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func (m *terminalManager) start(cmd *exec.Cmd, workspace, kind string) (*terminalSession, error) {
	return m.startSession(cmd, workspace, kind, claudeSelection{}, nil)
}

// This subprocess uses the same platform PTY implementation as workbench SSH.
func TestTerminalServiceChild(t *testing.T) {
	if os.Getenv("CAGE_TERMINAL_SERVICE_CHILD") != "1" {
		return
	}
	fmt.Println("terminal-ready")
	if os.Getenv("CAGE_TERMINAL_SERVICE_MODE") == "blocked-input" {
		time.Sleep(24 * time.Hour)
		os.Exit(0)
	}
	input := bufio.NewScanner(os.Stdin)
	for input.Scan() {
		line := input.Text()
		fmt.Println("child:" + line)
		if line == "exit" {
			os.Exit(0)
		}
	}
	os.Exit(0)
}

func terminalChildCommand(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestTerminalServiceChild$")
	cmd.Env = append(os.Environ(), "CAGE_TERMINAL_SERVICE_CHILD=1", "CAGE_TERMINAL_SERVICE_MODE="+mode)
	return cmd
}

func terminalInputLine(text string) string {
	if runtime.GOOS == "windows" {
		return text + "\r"
	}
	return text + "\n"
}

func awaitTerminalSignal(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("terminal did not finish")
	}
}

func readTerminalUntil(t *testing.T, sub *terminalSubscription, needle string) string {
	t.Helper()
	text := string(sub.History)
	sub.History = nil
	deadline := time.After(10 * time.Second)
	for !strings.Contains(text, needle) {
		select {
		case chunk, ok := <-sub.Output:
			if !ok {
				t.Fatalf("terminal ended before %q: %q", needle, text)
			}
			if chunk.InputStatus != "" {
				continue
			}
			text += string(chunk.Data)
		case <-deadline:
			t.Fatalf("terminal did not produce %q: %q", needle, text)
		}
	}
	return text
}

func TestTerminalServiceReconnectKeepsRealProcess(t *testing.T) {
	var manager terminalManager
	t.Cleanup(manager.close)
	session, err := manager.start(terminalChildCommand("echo"), "/project", "shell")
	if err != nil {
		t.Fatal(err)
	}
	first := session.subscribe()
	readTerminalUntil(t, first, "terminal-ready")
	if _, err := io.WriteString(session.file, terminalInputLine("one")); err != nil {
		t.Fatal(err)
	}
	readTerminalUntil(t, first, "child:one")
	first.Close()
	if !manager.hasLive() {
		t.Fatal("detaching viewer ended terminal process")
	}
	if _, err := io.WriteString(session.file, terminalInputLine("two")); err != nil {
		t.Fatal(err)
	}
	second := session.subscribe()
	defer second.Close()
	text := readTerminalUntil(t, second, "child:two")
	if !strings.Contains(text, "child:one") {
		t.Fatalf("reconnection lost history: %q", text)
	}
	if _, err := io.WriteString(session.file, terminalInputLine("exit")); err != nil {
		t.Fatal(err)
	}
	awaitTerminalSignal(t, session.done)
	if manager.hasLive() || !session.summary()["closed"].(bool) {
		t.Fatal("process exit was not published")
	}
	if err := session.file.Wait(); err != nil {
		t.Fatalf("child was killed instead of exiting normally: %v", err)
	}
}

func TestTerminalCloseUnblocksRealPTYInput(t *testing.T) {
	var manager terminalManager
	t.Cleanup(manager.close)
	session, err := manager.start(terminalChildCommand("blocked-input"), "/project", "shell")
	if err != nil {
		t.Fatal(err)
	}
	sub := session.subscribe()
	defer sub.Close()
	readTerminalUntil(t, sub, "terminal-ready")
	if err := session.file.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	writeStarted, writeDone := make(chan struct{}), make(chan struct{})
	go func() {
		close(writeStarted)
		_, _ = io.WriteString(session.file, strings.Repeat("input\n", 1<<20))
		close(writeDone)
	}()
	awaitTerminalSignal(t, writeStarted)
	select {
	case <-writeDone:
		t.Fatal("PTY input unexpectedly drained without a reader")
	case <-time.After(20 * time.Millisecond):
	}
	closed := make(chan struct{})
	go func() { manager.close(); close(closed) }()
	awaitTerminalSignal(t, closed)
	awaitTerminalSignal(t, writeDone)
	awaitTerminalSignal(t, session.done)
	if manager.hasLive() {
		t.Fatal("close returned while terminal was live")
	}
}

func TestTerminalManagerClosesConcurrentCreation(t *testing.T) {
	var manager terminalManager
	t.Cleanup(manager.close)
	begin, created, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var session *terminalSession
	var createErr error
	go func() {
		<-begin
		session, createErr = manager.start(terminalChildCommand("blocked-input"), "/project", "shell")
		close(created)
	}()
	go func() { <-begin; manager.close(); close(closed) }()
	close(begin)
	awaitTerminalSignal(t, created)
	awaitTerminalSignal(t, closed)
	if createErr != nil && !errors.Is(createErr, errTerminalsClosed) {
		t.Fatal(createErr)
	}
	if session != nil {
		awaitTerminalSignal(t, session.done)
	}
	if manager.hasLive() {
		t.Fatal("concurrent create published a live session after close")
	}
	if _, err := manager.start(terminalChildCommand("echo"), "", "shell"); !errors.Is(err, errTerminalsClosed) {
		t.Fatalf("closed manager accepted a process: %v", err)
	}
}

func TestTerminalManagerStopAllowsLaterTerminals(t *testing.T) {
	var manager terminalManager
	t.Cleanup(manager.close)
	first, err := manager.start(terminalChildCommand("blocked-input"), "/first", "shell")
	if err != nil {
		t.Fatal(err)
	}
	manager.closeAll()
	awaitTerminalSignal(t, first.done)
	if manager.get(first.id) != first || manager.hasLive() {
		t.Fatal("stop discarded closed history or kept process alive")
	}
	second, err := manager.start(terminalChildCommand("blocked-input"), "/second", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if second.title != "Claude 2" {
		t.Fatalf("terminal numbering reset: %s", second.title)
	}
	manager.remove(second.id)
	awaitTerminalSignal(t, second.done)
	if manager.get(second.id) != nil {
		t.Fatal("removed terminal remained visible")
	}
}

type controlledTerminal struct {
	reader      *io.PipeReader
	writer      *io.PipeWriter
	killed      chan struct{}
	waitStarted chan struct{}
	waitRelease chan struct{}
	killOnce    sync.Once
}

func newControlledSession() (*terminalSession, *controlledTerminal) {
	reader, writer := io.Pipe()
	file := &controlledTerminal{reader: reader, writer: writer, killed: make(chan struct{}), waitStarted: make(chan struct{}), waitRelease: make(chan struct{})}
	session := newTerminalSession(file)
	go session.run()
	return session, file
}
func (f *controlledTerminal) Read(data []byte) (int, error)  { return f.reader.Read(data) }
func (f *controlledTerminal) Write(data []byte) (int, error) { return len(data), nil }
func (f *controlledTerminal) Resize(uint16, uint16) error    { return nil }
func (f *controlledTerminal) Kill() error                    { f.killOnce.Do(func() { close(f.killed) }); return nil }
func (f *controlledTerminal) Wait() error                    { close(f.waitStarted); <-f.waitRelease; return nil }
func (f *controlledTerminal) Close() error                   { _ = f.writer.Close(); return f.reader.Close() }

func TestTerminalEOFReapsProcessBeforeSessionCompletes(t *testing.T) {
	session, file := newControlledSession()
	sub := session.subscribe()
	_ = file.writer.Close()
	awaitTerminalSignal(t, file.killed)
	awaitTerminalSignal(t, file.waitStarted)
	select {
	case <-session.done:
		t.Fatal("EOF completed session before process was reaped")
	default:
	}
	closed := make(chan struct{})
	go func() { session.close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("close returned before process Wait")
	default:
	}
	close(file.waitRelease)
	awaitTerminalSignal(t, closed)
	if _, ok := <-sub.Output; ok {
		t.Fatal("session completion did not close subscription")
	}
}

func TestTerminalSlowSubscriberDoesNotBlockOutput(t *testing.T) {
	session, file := newControlledSession()
	t.Cleanup(func() { close(file.waitRelease); session.close() })
	slow := session.subscribe()
	fast := session.subscribe()
	defer fast.Close()
	for i := 0; i < 66; i++ {
		chunk := fmt.Sprintf("output-%02d\n", i)
		if _, err := io.WriteString(file.writer, chunk); err != nil {
			t.Fatal(err)
		}
		readTerminalUntil(t, fast, chunk)
	}
	count := 0
	for range slow.Output {
		count++
	}
	if count != 64 {
		t.Fatalf("slow subscriber retained %d chunks", count)
	}
	fast.Close()
	if session.summary()["closed"].(bool) {
		t.Fatal("subscription closure ended session")
	}
	reconnected := session.subscribe()
	defer reconnected.Close()
	if !strings.Contains(string(reconnected.History), "output-65") {
		t.Fatal("slow client prevented recording terminal history")
	}
}

func TestTerminalQueuedInputKeepsAdmissionOrder(t *testing.T) {
	var manager terminalManager
	t.Cleanup(manager.close)
	session, err := manager.start(terminalChildCommand("echo"), "/project", "shell")
	if err != nil {
		t.Fatal(err)
	}
	sub := session.subscribe()
	defer sub.Close()
	readTerminalUntil(t, sub, "terminal-ready")
	for _, line := range []string{"first", "second", "third", "exit"} {
		if err := session.enqueueInput(context.Background(), terminalInputLine(line)); err != nil {
			t.Fatal(err)
		}
	}
	awaitTerminalSignal(t, session.done)
	reconnected := session.subscribe()
	defer reconnected.Close()
	text := string(reconnected.History)
	previous := -1
	for _, line := range []string{"first", "second", "third", "exit"} {
		index := strings.Index(text, "child:"+line)
		if index <= previous {
			t.Fatalf("queued input lost or reordered %s: %q", line, text)
		}
		previous = index
	}
}
