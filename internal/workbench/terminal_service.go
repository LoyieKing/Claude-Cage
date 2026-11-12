package workbench

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"al.essio.dev/pkg/shellescape"
)

var (
	errTerminalsClosed  = errors.New("后台正在退出，请重新打开 Cage。")
	errTerminalBusy     = errors.New("环境正在执行其他操作")
	errTerminalNotReady = errors.New("环境尚未就绪或正在重启")
	errTerminalLimit    = errors.New("最多保留 12 个终端，请先关闭不用的会话")
	errTerminalStart    = errors.New("无法创建终端连接")
)

type terminalRequest struct {
	Workspace       string `json:"workspace"`
	Kind            string `json:"kind"`
	ClaudeAccountID string `json:"claude_account_id"`
	sharedID        string
	requestContext  context.Context
}

// terminalManager owns host PTYs; subscriptions only borrow their output. The
// manager serializes process creation with closure so no process can be
// published after shutdown has collected the sessions it needs to close.
type terminalManager struct {
	mu        sync.Mutex
	sessions  map[string]*terminalSession
	sequence  int
	closed    bool
	workers   sync.WaitGroup
	stopGuest func(context.Context) error
}

func (m *terminalManager) startSession(cmd *exec.Cmd, workspace, kind string, selection claudeSelection, finished func()) (*terminalSession, error) {
	return m.startSessionWithPTY(cmd, workspace, kind, selection, finished, func() (terminalPTY, error) { return startTerminal(cmd, 100, 30) })
}

func (m *terminalManager) startSessionWithPTY(cmd *exec.Cmd, workspace, kind string, selection claudeSelection, finished func(), start func() (terminalPTY, error)) (*terminalSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.admissionErrorLocked(); err != nil {
		return nil, err
	}
	file, err := start()
	if err != nil {
		return nil, errTerminalStart
	}
	m.sequence++
	title := fmt.Sprintf("终端 %d", m.sequence)
	if kind == "claude" {
		title = fmt.Sprintf("Claude %d", m.sequence)
	} else if kind == "claude-login" {
		title = "Claude 账号登录"
	}
	session := newTerminalSession(file)
	session.id, session.title, session.workspace, session.cmd = randomToken()[:16], title, workspace, cmd
	session.kind, session.claudeAccountID = kind, selection.ID
	if kind == "claude-login" {
		session.loginGuestID = selection.GuestID
	}
	if selection.GuestID == "default" {
		session.claudeAccountSource = "default"
	} else if selection.ID != "" {
		session.claudeAccountSource = "managed"
	}
	if m.sessions == nil {
		m.sessions = make(map[string]*terminalSession)
	}
	m.sessions[session.id] = session
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		session.run()
		if finished != nil && !session.skipFinished() {
			finished()
		}
	}()
	return session, nil
}

func (m *terminalManager) admissionErrorLocked() error {
	if m.closed {
		return errTerminalsClosed
	}
	if len(m.sessions) >= 12 {
		return errTerminalLimit
	}
	return nil
}

func (m *terminalManager) canStart() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.admissionErrorLocked()
}

func (m *terminalManager) snapshot() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]map[string]any, 0, len(m.sessions))
	for _, session := range m.sessions {
		out = append(out, session.summary())
	}
	return out
}

func (m *terminalManager) hasLive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		session.mu.Lock()
		closed := session.closed
		session.mu.Unlock()
		if !closed {
			return true
		}
		if session.done != nil {
			select {
			case <-session.done:
			default:
				return true
			}
		}
	}
	return false
}

func (m *terminalManager) get(id string) *terminalSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *terminalManager) forConversation(connection string) *terminalSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		session.mu.Lock()
		matches := session.connectionID == connection && !session.closed
		session.mu.Unlock()
		if matches {
			return session
		}
	}
	return nil
}

// Reattaching a repeated login request must not create a second OAuth flow.
// A profile can serve many normal terminals or a single exclusive login.
func (m *terminalManager) existingClaudeLogin(id string, login bool) (*terminalSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		if session.claudeAccountID != id {
			continue
		}
		select {
		case <-session.done:
			continue
		default:
		}
		if session.kind == "claude-login" && login {
			return session, nil
		}
		if login || session.kind == "claude-login" {
			return nil, errors.New("请先结束此账号的登录或运行中的终端，再更改登录身份")
		}
	}
	return nil, nil
}

func (m *terminalManager) remove(id string) {
	m.mu.Lock()
	session := m.sessions[id]
	m.mu.Unlock()
	if session != nil {
		// Keep its Claude account reserved until the old PTY has exited.
		session.close()
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
	}
}

// closeAll keeps ended sessions visible until explicitly removed and permits
// new terminals after the environment starts again. Environment operations
// serialize this call with createTerminal using Server.op.
func (m *terminalManager) closeAll() {
	m.mu.Lock()
	sessions := make([]*terminalSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.Unlock()
	for _, session := range sessions {
		session.close()
	}
	m.mu.Lock()
	stop := m.stopGuest
	m.mu.Unlock()
	if stop != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		_ = stop(ctx)
		cancel()
	}
}

// beginDetach seals admission and marks persistent guest PTYs detach-only.
// It performs no waits and no network I/O. Call it before cancelling SSH.
func (m *terminalManager) beginDetach() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, session := range m.sessions {
		session.beginDetach()
	}
}

// detach reuses the detach-only mark, then closes attachments. Native PTYs are
// reaped. Guest shells stay running until explicit closeAll.
func (m *terminalManager) detach() {
	m.beginDetach()
	m.mu.Lock()
	sessions := make([]*terminalSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.Unlock()
	for _, session := range sessions {
		if session.persistsInGuest() {
			session.signalDetach()
			continue
		}
		session.close()
	}
}

func (m *terminalManager) adopt(session *terminalSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || session == nil || session.id == "" {
		return
	}
	if m.sessions == nil {
		m.sessions = map[string]*terminalSession{}
	}
	if m.sessions[session.id] != nil {
		return
	}
	m.sessions[session.id] = session
}

func (m *terminalManager) rekey(oldID, newID string) {
	if oldID == "" || newID == "" || oldID == newID {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[oldID]
	if session == nil || m.sessions[newID] != nil {
		return
	}
	delete(m.sessions, oldID)
	session.mu.Lock()
	session.id = newID
	session.mu.Unlock()
	m.sessions[newID] = session
}

func (m *terminalManager) close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.closeAll()
	// The final login-status observation also belongs to the session worker.
	m.workers.Wait()
}

func (s *Server) createTerminal(input terminalRequest) (*terminalSession, error) {
	if !s.op.TryLock() {
		return nil, errTerminalBusy
	}
	defer s.op.Unlock()
	return s.createTerminalLocked(input)
}

// Caller holds the environment operation lock through PTY publication.
func (s *Server) createTerminalLocked(input terminalRequest) (*terminalSession, error) {
	if s.windowsGuest != nil {
		return s.createWindowsTerminal(input)
	}
	var sharedRecord *mobileRecord
	if input.sharedID != "" {
		s.mobileCLI.mu.Lock()
		record, err := s.mobileCLI.record(s.profile, input.sharedID)
		s.mobileCLI.mu.Unlock()
		if err != nil {
			return nil, err
		}
		sharedRecord = record
		if record.Switch != nil && record.Switch.Phase == "preparing" {
			return nil, errors.New("账号交接尚未完成，请先重试或取消交接")
		}
		if record.Archived || record.Deleted {
			return nil, errors.New("此会话已归档或删除，请先从已归档会话恢复")
		}
		// The guest can be unreadable or not yet started while its host PTY
		// already owns this conversation. A stale snapshot cannot start a copy.
		if existing := s.terminals.get(record.TerminalID); existing != nil && existing.ownsConversation() {
			return existing, nil
		}
		if existing := s.terminals.get(record.TerminalID); existing != nil && existing.summary()["closed"] == true {
			s.terminals.remove(record.TerminalID)
		}
	}
	s.mu.Lock()
	closing := s.closing
	componentUpdate := s.guestUpdateRequired
	command := claudeTerminalCommand(s.preferences)
	s.mu.Unlock()
	if closing {
		return nil, errTerminalsClosed
	}
	if sharedRecord != nil && (!sharedCLILauncher(command) || sharedRecord.Launcher != "" && sharedRecord.Launcher != command) {
		return nil, errors.New("主机启动命令与原会话不一致，请恢复原配置后重试")
	}
	if s.macosGuest != nil {
		if input.Kind == "claude" && !sharedCLILauncher(command) {
			return nil, errors.New("macOS 客机支持 claude 或 ccs 配置名启动命令")
		}
		s.mu.Lock()
		ready := s.ready
		s.mu.Unlock()
		if !ready {
			return nil, errTerminalNotReady
		}
	}
	if !s.vmRunning() || s.busy() {
		return nil, errTerminalNotReady
	}
	// Reject a full manager before opening project shares. start checks again
	// atomically with process publication because shutdown can race this work.
	if input.Kind != "claude-login" {
		if err := s.terminals.canStart(); err != nil {
			return nil, err
		}
	}
	if input.Kind != "" && input.Kind != "shell" && input.Kind != "claude" && input.Kind != "claude-login" {
		return nil, errors.New("终端类型无效")
	}
	managed := input.Kind == "claude" || input.Kind == "claude-login"
	if !managed && input.ClaudeAccountID != "" {
		return nil, errors.New("普通终端不能指定 Claude 账号")
	}
	// Login works even when the last project is disconnected or has moved.
	// It always runs from the guest home, without loading project settings.
	workspace := s.workspace()
	var err error
	if input.Kind != "claude-login" {
		if s.macOSLegacyWorkspace(sharedRecord) != "" {
			workspace, err = s.projectWorkspace(sharedRecord.Workspace)
		} else {
			workspace, err = s.connectProject(input.Workspace, false)
		}
		if err != nil {
			return nil, err
		}
	}
	script := terminalScript(s.cliWorkspace(workspace), input.Kind, command)
	if s.macosGuest != nil {
		script = `cd -- "$HOME" && exec env TERM=xterm-256color COLORTERM=truecolor /bin/zsh -l`
		if input.Kind != "claude-login" {
			script = "exec /usr/local/libexec/cage-macos-terminal " + shellescape.Quote(macOSProjectPath(s.macosGuest.User, workspace))
		}
	}
	var selection claudeSelection
	var finished func()
	if managed {
		login := input.Kind == "claude-login"
		if sharedRecord != nil && sharedRecord.Terminal {
			selection = sharedRecord.Account
		} else {
			selection, err = s.selectTerminalClaudeAccount(command, input.ClaudeAccountID, login, workspace)
		}
		if err != nil {
			return nil, err
		}
		if selection.ID != "" {
			if componentUpdate {
				return nil, errGuestUpdateRequired
			}
			if !validClaudeID(selection.ID) || !validClaudeGuestID(selection.GuestID) || (login && selection.GuestID == "default") {
				return nil, errors.New("Claude 账号 ID 无效")
			}
			if login && s.mobileCLI.activeAccounts()[selection.ID] > 0 {
				return nil, errors.New("请先结束该账号正在执行的移动 CLI 轮次，再重新登录")
			}
			if existing, conflict := s.terminals.existingClaudeLogin(selection.ID, login); existing != nil || conflict != nil {
				return existing, conflict
			}
			if !login && selection.Subject == "" {
				return nil, errors.New("Claude 账号状态已变化，请重试")
			}
			script = managedClaudeTerminalScript(s.cliWorkspace(workspace), command, selection.GuestID, selection.Subject, login)
			if login {
				if s.macosGuest != nil {
					ctx, cancel := context.WithTimeout(s.lifetimeContext(), 20*time.Second)
					_, browserErr := s.guestAPI(ctx, "", "GET", "/health", nil)
					cancel()
					if browserErr != nil {
						return nil, errors.New("客机登录浏览器尚未就绪，请恢复浏览器连接后重试")
					}
				}
				finished = func() {
					ctx, cancel := context.WithTimeout(s.lifetimeContext(), 12*time.Second)
					defer cancel()
					_ = s.confirmClaudeLogin(ctx, selection.ID)
				}
			}
		}
	}
	var conversation *mobileRecord
	if input.Kind == "claude" && sharedCLILauncher(command) {
		script, conversation, err = s.prepareTerminalConversation(workspace, selection, input.sharedID)
		if err != nil {
			return nil, err
		}
	}
	cmd := s.sshCommand(s.lifetimeContext(), "-tt", s.sshTarget(), "--", script)
	if input.requestContext != nil {
		s.account.mu.Lock()
		if !s.mobileAuthorized(input.requestContext) {
			s.account.mu.Unlock()
			return nil, errMobileLocked
		}
	}
	var session *terminalSession
	if s.macosGuest == nil {
		session, err = s.startPersistentGuestShell(guestShellSpec{
			Script: script, Workspace: workspace, Kind: input.Kind, Selection: selection, Finished: finished,
			Login: input.Kind == "claude-login",
		})
	} else {
		session, err = s.terminals.startSession(cmd, workspace, input.Kind, selection, finished)
	}
	if input.requestContext != nil {
		s.account.mu.Unlock()
	}
	if err == nil && conversation != nil {
		session.mu.Lock()
		session.sharedID = conversation.ID
		session.connectionID = conversation.Connection
		session.mu.Unlock()
		if session.persistsInGuest() {
			s.noteGuestShellConversation(session.id, conversation.ID, conversation.Connection)
		}
		s.mobileCLI.mu.Lock()
		conversation.TerminalID = session.id
		if saveErr := writePrivate(mobileRecordPath(s.profile, conversation.ID), conversation); saveErr != nil {
			err = saveErr
		}
		s.mobileCLI.mu.Unlock()
		if err != nil {
			s.terminals.remove(session.id)
			return nil, err
		}
		s.prepareCLIConnection(session, conversation.ID)
		s.monitorClaudeAccount(session, conversation.ID)
	}
	if err == nil && input.Kind == "claude" && selection.ID != "" {
		s.noteClaudeSelection(selection.ID, workspace)
	}
	return session, err
}

func validClaudeGuestID(id string) bool { return id == "default" || validClaudeID(id) }

func (s *Server) selectTerminalClaudeAccount(command, requested string, login bool, workspace string) (claudeSelection, error) {
	parts, valid := mobileLaunchArgs(command, claudeSelection{})
	if !login && valid == nil && len(parts) == 2 && parts[0] == "ccs" {
		if requested != "" && requested != "auto" {
			return claudeSelection{}, errors.New("CCS 使用自己的配置，不能指定 Cage 账号")
		}
		return claudeSelection{}, nil
	}
	return s.selectClaudeAccount(requested, login, workspace)
}

func (s *Server) readClaudeGuestSnapshot(ctx context.Context, id string) (claudeGuestSnapshot, error) {
	if s.windowsGuest != nil {
		return s.readWindowsClaudeAccount(ctx, "snapshot", id, "")
	}
	return s.readClaudeGuestReport(ctx, id, claudeSnapshotCommand(id))
}

func (s *Server) readClaudeGuestUsage(ctx context.Context, id, subject string) (claudeGuestSnapshot, error) {
	if _, err := hex.DecodeString(subject); err != nil || len(subject) != 64 {
		return claudeGuestSnapshot{}, errors.New("Claude 账号身份无效")
	}
	if s.windowsGuest != nil {
		return s.readWindowsClaudeAccount(ctx, "usage", id, subject)
	}
	return s.readClaudeGuestReport(ctx, id, claudeGuestReportCommand(id, subject))
}

func (s *Server) readClaudeGuestReport(ctx context.Context, id, command string) (claudeGuestSnapshot, error) {
	var snapshot claudeGuestSnapshot
	if !validClaudeGuestID(id) {
		return snapshot, errors.New("Claude 账号 ID 无效")
	}
	data, err := s.guest(ctx, command)
	if err != nil || json.Unmarshal(data, &snapshot) != nil {
		return snapshot, errors.New("无法读取账号状态，请确认环境已启动并同步环境组件")
	}
	return snapshot, nil
}

func claudeSnapshotCommand(id string) string {
	return claudeGuestReportCommand(id, "")
}

func claudeGuestReportCommand(id, subject string) string {
	command := "exec /usr/local/bin/cage-claude-account snapshot " + id
	if subject != "" {
		command = "exec /usr/local/bin/cage-claude-account usage " + id + " " + shellescape.Quote(subject)
	}
	if id == "default" {
		// Use the same login shell a regular terminal opens, including its
		// configured API/provider environment. Isolate startup banners from
		// the JSON output and keep the probe outside every shared project.
		command = `set -o pipefail; cd "$HOME" && env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 /bin/bash -lic ` +
			shellescape.Quote(`printf '\nCAGE_CLAUDE_SNAPSHOT '; `+command) +
			` | sed -n 's/^CAGE_CLAUDE_SNAPSHOT //p'`
	}
	return command
}

func managedClaudeTerminalScript(workspace, command, accountID, subject string, login bool) string {
	if login {
		return "cd \"$HOME\" && exec /usr/local/bin/cage-claude-account login " + shellescape.Quote(accountID)
	}
	// The interactive shell still interprets the user's saved command and
	// remains useful when Claude exits. The shim scopes subsequent invocations
	// in this shell to the same official CLI profile.
	env := `export CAGE_CLAUDE_ACCOUNT_ID=` + shellescape.Quote(accountID) +
		` CAGE_CLAUDE_SUBJECT=` + shellescape.Quote(subject) + "; "
	if accountID != "default" {
		env += `export CLAUDE_CONFIG_DIR="$HOME/.local/share/cage/claude-accounts/$CAGE_CLAUDE_ACCOUNT_ID/config"; `
	}
	env += `export PATH="/usr/local/lib/cage-claude-bin:$PATH"; `
	body := env + `eval "$CAGE_TERMINAL_COMMAND"; unset CAGE_TERMINAL_COMMAND; exec bash -i`
	return "cd " + shellescape.Quote(workspace) +
		" && exec env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 CAGE_TERMINAL_COMMAND=" +
		shellescape.Quote(command) + " bash -lic " + shellescape.Quote(body)
}

// The saved command is interpreted only by the guest's interactive shell.
// Reconnecting a viewer never executes it again; exiting Claude leaves a shell.
func terminalScript(workspace, kind, command string) string {
	prefix := "cd " + shellescape.Quote(workspace) + " && exec env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 "
	if kind != "claude" {
		return prefix + "bash -l"
	}
	return prefix + "CAGE_TERMINAL_COMMAND=" + shellescape.Quote(command) + " bash -lic " + shellescape.Quote("eval \"$CAGE_TERMINAL_COMMAND\"; unset CAGE_TERMINAL_COMMAND; exec bash -il")
}
