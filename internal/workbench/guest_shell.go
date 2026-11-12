package workbench

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"crypto/rand"
	_ "embed"

	"golang.org/x/crypto/ssh"
)

//go:embed guest_shell.py
var guestShellDaemonSource string

const (
	gsHello byte = 1 + iota
	gsResult
	gsCreate
	gsList
	gsAttach
	gsInput
	gsResize
	gsStop
	gsOutput
	gsGap
	gsExit
	gsLost
	gsError
	gsAttached
)

const (
	guestShellSocket = "/run/cage/guest-shell.sock"
	guestShellToken  = "/var/lib/cage/guest-shell.token"
	guestShellMax    = 1 << 20
)

type guestShellDialFunc func(context.Context) (io.ReadWriteCloser, error)

var (
	guestShellDialMu sync.Mutex
	guestShellDials  = map[*Server]guestShellDialFunc{}
	guestShellMetaMu sync.Mutex
)

func setGuestShellDial(s *Server, fn guestShellDialFunc) {
	guestShellDialMu.Lock()
	defer guestShellDialMu.Unlock()
	if fn == nil {
		delete(guestShellDials, s)
	} else {
		guestShellDials[s] = fn
	}
}

func (s *Server) guestShellDial(ctx context.Context) (io.ReadWriteCloser, error) {
	guestShellDialMu.Lock()
	fn := guestShellDials[s]
	guestShellDialMu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	client, err := s.dialGuestSSH(ctx)
	if err != nil {
		return nil, err
	}
	session, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		_ = client.Close()
		return nil, err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		_ = client.Close()
		return nil, err
	}
	session.Stderr = io.Discard
	if err = session.Start("/usr/local/bin/cage-guest-shell connect"); err != nil {
		_ = session.Close()
		_ = client.Close()
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	return &guestShellPipe{stdin: stdin, stdout: stdout, session: session, client: client, stop: stop}, nil
}

type guestShellPipe struct {
	stdin   io.WriteCloser
	stdout  io.Reader
	session *ssh.Session
	client  *ssh.Client
	stop    func() bool
	once    sync.Once
}

func (p *guestShellPipe) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *guestShellPipe) Write(b []byte) (int, error) { return p.stdin.Write(b) }
func (p *guestShellPipe) Close() error {
	var err error
	p.once.Do(func() {
		if p.stop != nil {
			p.stop()
		}
		// Close the connection first so a stuck channel copy cannot block
		// stdin or Wait. Then retire the session's copy pump.
		if p.client != nil {
			err = p.client.Close()
		}
		if p.stdin != nil {
			err = errors.Join(err, p.stdin.Close())
		}
		if p.session != nil {
			err = errors.Join(err, p.session.Close(), p.session.Wait())
		}
	})
	return err
}

type guestShellRecord struct {
	HostID              string `json:"host_id,omitempty"`
	CreateID            string `json:"create_id"`
	ShellID             string `json:"shell_id,omitempty"`
	Epoch               string `json:"epoch,omitempty"`
	PID                 int    `json:"pid,omitempty"`
	Generation          string `json:"generation,omitempty"`
	Workspace           string `json:"workspace,omitempty"`
	GuestCWD            string `json:"guest_cwd,omitempty"`
	Kind                string `json:"kind,omitempty"`
	Title               string `json:"title,omitempty"`
	ClaudeAccountID     string `json:"claude_account_id,omitempty"`
	ClaudeAccountSource string `json:"claude_account_source,omitempty"`
	LoginGuestID        string `json:"login_guest_id,omitempty"`
	SharedID            string `json:"shared_id,omitempty"`
	ConnectionID        string `json:"connection_id,omitempty"`
	Cols                uint16 `json:"cols,omitempty"`
	Rows                uint16 `json:"rows,omitempty"`
	ResizeSeq           uint64 `json:"resize_seq,omitempty"`
	Login               bool   `json:"login,omitempty"`
}

type guestShellCatalog struct {
	Shells []guestShellRecord `json:"shells,omitempty"`
}

type guestShellInfo struct {
	ShellID    string `json:"shell_id"`
	CreateID   string `json:"create_id"`
	PID        int    `json:"pid"`
	Generation string `json:"generation"`
	Epoch      string `json:"epoch"`
	Cols       int    `json:"cols"`
	Rows       int    `json:"rows"`
	ResizeSeq  uint64 `json:"resize_seq"`
	State      string `json:"state"`
	Login      bool   `json:"login"`
	Offset     uint64 `json:"offset"`
}

type guestShellSpec struct {
	Script    string
	Workspace string
	Kind      string
	Selection claudeSelection
	Finished  func()
	Login     bool
}

func guestShellCatalogPath(profile string) string {
	return filepath.Join(profile, "guest-shells.json")
}

func loadGuestShellCatalog(profile string) (guestShellCatalog, error) {
	guestShellMetaMu.Lock()
	defer guestShellMetaMu.Unlock()
	return loadGuestShellCatalogLocked(profile)
}

func loadGuestShellCatalogLocked(profile string) (guestShellCatalog, error) {
	var cat guestShellCatalog
	data, err := os.ReadFile(guestShellCatalogPath(profile))
	if os.IsNotExist(err) {
		return cat, nil
	}
	if err != nil {
		return cat, err
	}
	if len(data) > 1<<20 || json.Unmarshal(data, &cat) != nil {
		return guestShellCatalog{}, errors.New("客机终端记录无法读取")
	}
	return cat, nil
}

func saveGuestShellCatalog(profile string, cat guestShellCatalog) error {
	guestShellMetaMu.Lock()
	defer guestShellMetaMu.Unlock()
	return saveGuestShellCatalogLocked(profile, cat)
}

func saveGuestShellCatalogLocked(profile string, cat guestShellCatalog) error {
	if len(cat.Shells) == 0 {
		err := os.Remove(guestShellCatalogPath(profile))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return writePrivate(guestShellCatalogPath(profile), cat)
}

func upsertGuestShellRecord(profile string, rec guestShellRecord) error {
	guestShellMetaMu.Lock()
	defer guestShellMetaMu.Unlock()
	cat, err := loadGuestShellCatalogLocked(profile)
	if err != nil {
		return err
	}
	replaced := false
	for i := range cat.Shells {
		if cat.Shells[i].CreateID == rec.CreateID {
			cat.Shells[i] = rec
			replaced = true
			break
		}
	}
	if !replaced {
		cat.Shells = append(cat.Shells, rec)
	}
	return saveGuestShellCatalogLocked(profile, cat)
}

func guestShellTokenOK(token string) bool {
	if len(token) != 64 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

func (s *Server) guestShellToken() (string, error) {
	path := filepath.Join(s.profile, "guest-shell-token")
	if data, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(data))
		if guestShellTokenOK(token) {
			return token, nil
		}
		return "", errors.New("客机终端服务凭据无效")
	} else if !os.IsNotExist(err) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		return "", err
	}
	return token, nil
}

func writeFrame(w io.Writer, kind byte, payload []byte) error {
	body := append([]byte{kind}, payload...)
	if len(body) > guestShellMax {
		return errors.New("guest shell frame")
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

func writeJSONFrame(w io.Writer, kind byte, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeFrame(w, kind, payload)
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n < 1 || n > guestShellMax {
		return 0, nil, errors.New("guest shell frame")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return buf[0], buf[1:], nil
}

func guestShellHello(conn io.ReadWriteCloser, token string) (string, error) {
	if err := writeJSONFrame(conn, gsHello, map[string]string{"token": token}); err != nil {
		return "", err
	}
	kind, payload, err := readFrame(conn)
	if err != nil {
		return "", err
	}
	if kind == gsError {
		return "", errors.New("客机终端服务拒绝了连接")
	}
	if kind != gsResult {
		return "", errors.New("客机终端服务响应无效")
	}
	var result struct {
		Epoch string `json:"epoch"`
	}
	if json.Unmarshal(payload, &result) != nil || result.Epoch == "" {
		return "", errors.New("客机终端服务响应无效")
	}
	return result.Epoch, nil
}

func guestShellCall(conn io.ReadWriteCloser, kind byte, value any, out any) error {
	if err := writeJSONFrame(conn, kind, value); err != nil {
		return err
	}
	got, payload, err := readFrame(conn)
	if err != nil {
		return err
	}
	switch got {
	case gsError:
		return errors.New("客机终端服务拒绝了请求")
	case gsLost:
		return errTerminalLost
	case gsResult:
		if out == nil {
			return nil
		}
		if json.Unmarshal(payload, out) != nil {
			return errors.New("客机终端服务响应无效")
		}
		return nil
	default:
		return errors.New("客机终端服务响应无效")
	}
}

func (s *Server) withGuestShell(ctx context.Context, token string, fn func(io.ReadWriteCloser, string) error) error {
	conn, err := s.guestShellDial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	epoch, err := guestShellHello(conn, token)
	if err != nil {
		return err
	}
	return fn(conn, epoch)
}

func (s *Server) serverClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

func (s *Server) ensureGuestShellService(ctx context.Context) error {
	if s.serverClosing() || ctx.Err() != nil {
		return errTerminalsClosed
	}
	token, err := s.guestShellToken()
	if err != nil {
		return err
	}
	err = s.withGuestShell(ctx, token, func(io.ReadWriteCloser, string) error { return nil })
	if err == nil {
		return nil
	}
	if s.serverClosing() || ctx.Err() != nil {
		return errTerminalsClosed
	}
	if err = s.provisionGuestShell(ctx, token); err != nil {
		return errGuestUpdateRequired
	}
	if err = s.withGuestShell(ctx, token, func(io.ReadWriteCloser, string) error { return nil }); err != nil {
		return errGuestUpdateRequired
	}
	return nil
}

func (s *Server) provisionGuestShell(ctx context.Context, token string) error {
	if !guestShellTokenOK(token) || s.serverClosing() || ctx.Err() != nil {
		return errGuestUpdateRequired
	}
	client, err := s.dialGuestSSH(ctx)
	if err != nil {
		return errGuestUpdateRequired
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return errGuestUpdateRequired
	}
	defer session.Close()
	session.Stdin = strings.NewReader(guestShellInstallScript(guestShellDaemonSource, token))
	session.Stdout = io.Discard
	session.Stderr = io.Discard
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	if err = session.Run("exec /bin/bash --noprofile --norc -se"); err != nil || ctx.Err() != nil {
		return errGuestUpdateRequired
	}
	return nil
}

func guestShellInstallScript(source, token string) string {
	if !strings.HasSuffix(source, "\n") {
		source += "\n"
	}
	return strings.Join([]string{
		"set -euo pipefail",
		"umask 077",
		"user=$(id -un)",
		"group=$(id -gn)",
		`if [ "$(id -u)" -eq 0 ]; then exit 1; fi`,
		"token='" + token + "'",
		"tmp=$(mktemp)",
		"trap 'rm -f \"$tmp\"' EXIT",
		"cat >\"$tmp\" <<'CAGE_GUEST_SHELL_PY'",
		source + "CAGE_GUEST_SHELL_PY",
		"sudo -n install -d -o root -g root -m 0755 /usr/local/lib/cage /usr/local/bin",
		"sudo -n install -d -o \"$user\" -g \"$group\" -m 0700 /var/lib/cage",
		"sudo -n install -o root -g root -m 0644 \"$tmp\" /usr/local/lib/cage/guest_shell.py",
		"printf '%s\\n' \"$token\" | sudo -n tee " + guestShellToken + " >/dev/null",
		"sudo -n chown \"$user:$group\" " + guestShellToken,
		"sudo -n chmod 0600 " + guestShellToken,
		"sudo -n tee /usr/local/bin/cage-guest-shell >/dev/null <<'EOF'",
		"#!/bin/sh",
		"export CAGE_GUEST_SHELL_SOCK=" + guestShellSocket,
		"exec /usr/bin/python3 -I /usr/local/lib/cage/guest_shell.py connect",
		"EOF",
		"sudo -n chmod 0755 /usr/local/bin/cage-guest-shell",
		"sudo -n tee /etc/systemd/system/cage-guest-shell.service >/dev/null <<EOF",
		"[Unit]",
		"Description=Cage guest shell",
		"[Service]",
		"Type=simple",
		"User=$user",
		"Group=$group",
		"UMask=0077",
		"RuntimeDirectory=cage",
		"RuntimeDirectoryMode=0700",
		"StateDirectory=cage",
		"StateDirectoryMode=0700",
		"Environment=CAGE_GUEST_SHELL_SOCK=" + guestShellSocket,
		"Environment=CAGE_GUEST_SHELL_TOKEN_FILE=" + guestShellToken,
		"ExecStart=/usr/bin/python3 -I /usr/local/lib/cage/guest_shell.py serve",
		"Restart=on-failure",
		"RestartSec=1",
		"KillMode=process",
		"[Install]",
		"WantedBy=multi-user.target",
		"EOF",
		"sudo -n systemctl daemon-reload",
		"if ! sudo -n systemctl is-active --quiet cage-guest-shell.service; then sudo -n systemctl enable --now cage-guest-shell.service; fi",
		"",
	}, "\n")
}

func (s *Server) startPersistentGuestShell(spec guestShellSpec) (*terminalSession, error) {
	if s.macosGuest != nil || s.windowsGuest != nil {
		return nil, errors.New("原生终端不使用客机 shell 服务")
	}
	ctx := s.lifetimeContext()
	if s.serverClosing() || ctx.Err() != nil {
		return nil, errTerminalsClosed
	}
	if err := s.ensureGuestShellService(ctx); err != nil {
		return nil, err
	}
	token, err := s.guestShellToken()
	if err != nil {
		return nil, err
	}
	s.bindGuestShellStop()
	createID := randomToken()
	rec := guestShellRecord{
		CreateID: createID, Workspace: spec.Workspace, Kind: spec.Kind, Login: spec.Login,
		Cols: 100, Rows: 30, ClaudeAccountID: spec.Selection.ID, ClaudeAccountSource: accountSource(spec.Selection),
		LoginGuestID: spec.Selection.GuestID,
	}
	if !spec.Login {
		rec.GuestCWD = guestPath(spec.Workspace)
	}
	if err = upsertGuestShellRecord(s.profile, rec); err != nil {
		return nil, err
	}
	info, epoch, sureMiss, err := s.createGuestShell(ctx, token, rec, spec.Script)
	if err != nil {
		if sureMiss {
			_ = deleteGuestShellRecord(s.profile, createID)
		}
		if s.serverClosing() || ctx.Err() != nil {
			return nil, errTerminalsClosed
		}
		return nil, errTerminalStart
	}
	rec.ShellID, rec.PID, rec.Generation, rec.Epoch = info.ShellID, info.PID, info.Generation, epoch
	rec.ResizeSeq = info.ResizeSeq
	if info.Cols > 0 && info.Rows > 0 {
		rec.Cols, rec.Rows = uint16(info.Cols), uint16(info.Rows)
	}
	if err = upsertGuestShellRecord(s.profile, rec); err != nil {
		return nil, err
	}
	pty := newGuestShellPTY(s.guestShellDial, token, rec, ctx)
	pty.profile = s.profile
	session, err := s.terminals.startSessionWithPTY(nil, spec.Workspace, spec.Kind, spec.Selection, spec.Finished, func() (terminalPTY, error) {
		return pty, nil
	})
	if err != nil {
		// The shell was created but never published. Keep it only when the
		// controller is closing and the catalog record can be restored.
		if s.serverClosing() || ctx.Err() != nil {
			pty.detach()
			return nil, err
		}
		_ = pty.Kill()
		_ = deleteGuestShellRecord(s.profile, rec.CreateID)
		return nil, err
	}
	rec.HostID = session.id
	rec.Title, _ = session.summary()["title"].(string)
	if err = upsertGuestShellRecord(s.profile, rec); err != nil {
		s.terminals.remove(session.id)
		return nil, err
	}
	return session, nil
}

func accountSource(selection claudeSelection) string {
	if selection.GuestID == "default" {
		return "default"
	}
	if selection.ID != "" {
		return "managed"
	}
	return ""
}

func (s *Server) createGuestShell(ctx context.Context, token string, rec guestShellRecord, script string) (guestShellInfo, string, bool, error) {
	var info guestShellInfo
	var epoch string
	request := map[string]any{"create_id": rec.CreateID, "script": script, "cwd": rec.GuestCWD, "cols": rec.Cols, "rows": rec.Rows, "login": rec.Login}
	err := s.withGuestShell(ctx, token, func(conn io.ReadWriteCloser, got string) error {
		epoch = got
		return guestShellCall(conn, gsCreate, request, &info)
	})
	if err == nil && info.ShellID != "" {
		return info, epoch, false, nil
	}
	found, listEpoch, listErr := s.findGuestShell(ctx, token, rec.CreateID)
	if listErr == nil && found.ShellID != "" {
		return found, listEpoch, false, nil
	}
	if listErr == nil && epoch != "" && listEpoch != "" && listEpoch != epoch {
		return guestShellInfo{}, listEpoch, false, errors.New("客机终端服务已重启，创建结果未知")
	}
	sure := false
	if ctx.Err() != nil || s.serverClosing() {
		return guestShellInfo{}, epoch, false, err
	}
	err = s.withGuestShell(ctx, token, func(conn io.ReadWriteCloser, got string) error {
		epoch = got
		return guestShellCall(conn, gsCreate, request, &info)
	})
	if err == nil && info.ShellID != "" {
		return info, epoch, false, nil
	}
	found, listEpoch, listErr = s.findGuestShell(ctx, token, rec.CreateID)
	if listErr == nil && found.ShellID != "" {
		return found, listEpoch, false, nil
	}
	if listErr == nil {
		sure = true
	}
	return guestShellInfo{}, epoch, sure, err
}

func (s *Server) findGuestShell(ctx context.Context, token, createID string) (guestShellInfo, string, error) {
	var found guestShellInfo
	var epoch string
	err := s.withGuestShell(ctx, token, func(conn io.ReadWriteCloser, got string) error {
		epoch = got
		var listed struct {
			Epoch  string           `json:"epoch"`
			Shells []guestShellInfo `json:"shells"`
		}
		if err := guestShellCall(conn, gsList, map[string]any{}, &listed); err != nil {
			return err
		}
		for _, item := range listed.Shells {
			if item.CreateID == createID {
				found = item
				return nil
			}
		}
		return nil
	})
	return found, epoch, err
}

func deleteGuestShellRecord(profile, createID string) error {
	guestShellMetaMu.Lock()
	defer guestShellMetaMu.Unlock()
	cat, err := loadGuestShellCatalogLocked(profile)
	if err != nil {
		return err
	}
	next := cat.Shells[:0]
	for _, rec := range cat.Shells {
		if rec.CreateID != createID {
			next = append(next, rec)
		}
	}
	cat.Shells = next
	return saveGuestShellCatalogLocked(profile, cat)
}

func (s *Server) noteGuestShellConversation(hostID, sharedID, connectionID string) {
	if hostID == "" {
		return
	}
	guestShellMetaMu.Lock()
	defer guestShellMetaMu.Unlock()
	cat, err := loadGuestShellCatalogLocked(s.profile)
	if err != nil {
		return
	}
	for i := range cat.Shells {
		if cat.Shells[i].HostID == hostID {
			cat.Shells[i].SharedID = sharedID
			cat.Shells[i].ConnectionID = connectionID
			_ = saveGuestShellCatalogLocked(s.profile, cat)
			return
		}
	}
}

func (s *Server) bindGuestShellStop() {
	s.terminals.mu.Lock()
	defer s.terminals.mu.Unlock()
	if s.terminals.stopGuest == nil {
		s.terminals.stopGuest = s.stopRecordedGuestShells
	}
}

func (s *Server) stopRecordedGuestShells(ctx context.Context) error {
	cat, err := loadGuestShellCatalog(s.profile)
	if err != nil || len(cat.Shells) == 0 {
		return err
	}
	token, err := s.guestShellToken()
	if err != nil {
		return err
	}
	var joined error
	for _, rec := range cat.Shells {
		if rec.ShellID == "" {
			continue
		}
		err = s.withGuestShell(ctx, token, func(conn io.ReadWriteCloser, _ string) error {
			var result struct {
				Stopped bool `json:"stopped"`
			}
			return guestShellCall(conn, gsStop, map[string]any{"shell_id": rec.ShellID, "epoch": rec.Epoch, "pid": rec.PID, "generation": rec.Generation}, &result)
		})
		if err != nil && !errors.Is(err, errTerminalLost) {
			joined = errors.Join(joined, err)
		}
	}
	if joined == nil {
		_ = saveGuestShellCatalog(s.profile, guestShellCatalog{})
	}
	return joined
}

func (s *Server) restoreGuestTerminals(ctx context.Context) error {
	if s.macosGuest != nil || s.windowsGuest != nil || s.serverClosing() {
		return nil
	}
	cat, err := loadGuestShellCatalog(s.profile)
	if err != nil || len(cat.Shells) == 0 {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.bindGuestShellStop()
	token, err := s.guestShellToken()
	if err != nil {
		return err
	}
	var listed struct {
		Epoch  string           `json:"epoch"`
		Shells []guestShellInfo `json:"shells"`
	}
	var epoch string
	err = s.withGuestShell(ctx, token, func(conn io.ReadWriteCloser, got string) error {
		epoch = got
		return guestShellCall(conn, gsList, map[string]any{}, &listed)
	})
	if err != nil {
		return err
	}
	byID := map[string]guestShellInfo{}
	byCreate := map[string]guestShellInfo{}
	for _, item := range listed.Shells {
		byID[item.ShellID] = item
		if item.CreateID != "" {
			byCreate[item.CreateID] = item
		}
	}
	var kept []guestShellRecord
	for _, rec := range cat.Shells {
		if rec.Epoch != "" && rec.Epoch != epoch {
			s.adoptLostTerminal(rec)
			continue
		}
		if rec.ShellID == "" {
			found, ok := byCreate[rec.CreateID]
			if !ok {
				s.adoptLostTerminal(rec)
				continue
			}
			rec.ShellID, rec.PID, rec.Generation, rec.Epoch = found.ShellID, found.PID, found.Generation, found.Epoch
		}
		found, ok := byID[rec.ShellID]
		if !ok || found.PID != rec.PID || found.Generation != rec.Generation || found.Epoch != epoch {
			s.adoptLostTerminal(rec)
			continue
		}
		if found.State == "stopped" || found.State == "exited" {
			continue
		}
		if found.State != "running" && found.State != "starting" {
			s.adoptLostTerminal(rec)
			continue
		}
		if !rec.Login {
			if _, err = s.projectWorkspace(rec.Workspace); err != nil {
				kept = append(kept, rec)
				continue
			}
		}
		rec.Epoch = epoch
		rec.ResizeSeq = found.ResizeSeq
		if found.Cols > 0 && found.Rows > 0 {
			rec.Cols, rec.Rows = uint16(found.Cols), uint16(found.Rows)
		}
		if err = s.adoptRunningGuestShell(&rec, token); err != nil {
			return err
		}
		kept = append(kept, rec)
	}
	return saveGuestShellCatalog(s.profile, guestShellCatalog{Shells: kept})
}

func (s *Server) adoptLostTerminal(rec guestShellRecord) {
	id := rec.HostID
	if id == "" {
		id = rec.ShellID
	}
	if id == "" {
		id = rec.CreateID
	}
	session := &terminalSession{
		id: id, title: rec.Title, workspace: rec.Workspace, kind: rec.Kind,
		claudeAccountID: rec.ClaudeAccountID, claudeAccountSource: rec.ClaudeAccountSource,
		loginGuestID: rec.LoginGuestID, sharedID: rec.SharedID, connectionID: rec.ConnectionID,
		clients: map[chan terminalFrame]bool{}, detached: make(chan struct{}), state: "lost", closed: true,
		done: make(chan struct{}),
	}
	if session.title == "" {
		session.title = "终端已丢失"
	}
	close(session.done)
	s.terminals.adopt(session)
}

func (s *Server) adoptRunningGuestShell(rec *guestShellRecord, token string) error {
	if s.serverClosing() {
		return errTerminalsClosed
	}
	if rec.HostID != "" && s.terminals.get(rec.HostID) != nil {
		return nil
	}
	pty := newGuestShellPTY(s.guestShellDial, token, *rec, s.lifetimeContext())
	pty.profile = s.profile
	session, err := s.terminals.startSessionWithPTY(nil, rec.Workspace, rec.Kind, claudeSelection{ID: rec.ClaudeAccountID, GuestID: rec.LoginGuestID}, nil, func() (terminalPTY, error) {
		return pty, nil
	})
	if err != nil {
		pty.detach()
		return err
	}
	if rec.HostID == "" {
		rec.HostID = session.id
	} else if rec.HostID != session.id {
		s.terminals.rekey(session.id, rec.HostID)
	}
	session.mu.Lock()
	session.id = rec.HostID
	if rec.Title != "" {
		session.title = rec.Title
	}
	session.sharedID = rec.SharedID
	session.connectionID = rec.ConnectionID
	session.mu.Unlock()
	return nil
}

type guestShellPTY struct {
	dial        guestShellDialFunc
	token       string
	profile     string
	record      guestShellRecord
	life        context.Context
	mu          sync.Mutex
	cond        *sync.Cond
	conn        io.ReadWriteCloser
	chunks      []shellOutput
	gap         terminalGap
	gapPending  bool
	fatal       error
	offset      uint64
	replayUntil uint64
	replaySet   bool
	lastReplay  bool
	detachOnly  atomic.Bool
	killed      atomic.Bool
	exited      chan struct{}
	waitErr     error
	finishOnce  sync.Once
	resizeSeq   atomic.Uint64
	inputs      map[string]string
	inputOrder  []string
	inputMu     sync.Mutex
}

type shellOutput struct {
	data   []byte
	off    int
	replay bool
}

const guestInputRemember = 1024

func newGuestShellPTY(dial guestShellDialFunc, token string, rec guestShellRecord, life context.Context) *guestShellPTY {
	p := &guestShellPTY{dial: dial, token: token, record: rec, life: life, exited: make(chan struct{}), inputs: map[string]string{}}
	p.cond = sync.NewCond(&p.mu)
	p.resizeSeq.Store(rec.ResizeSeq)
	if life == nil {
		p.life = context.Background()
	}
	go p.loop()
	return p
}

func (p *guestShellPTY) beginDetach() { p.detachOnly.Store(true) }

func (p *guestShellPTY) detach() {
	p.beginDetach()
	p.closeConn()
	p.fail(errTerminalDetached)
}

func (p *guestShellPTY) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if p.gapPending {
			return 0, errTerminalGap
		}
		if len(p.chunks) > 0 {
			chunk := &p.chunks[0]
			n := copy(b, chunk.data[chunk.off:])
			chunk.off += n
			p.lastReplay = chunk.replay
			if chunk.off == len(chunk.data) {
				p.chunks = p.chunks[1:]
			}
			return n, nil
		}
		if p.fatal != nil {
			return 0, p.fatal
		}
		p.cond.Wait()
	}
}

func (p *guestShellPTY) lastReadReplay() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastReplay
}

func (p *guestShellPTY) queueOutputLocked(offset uint64, data []byte) {
	if len(data) == 0 {
		if offset > p.offset {
			p.offset = offset
		}
		return
	}
	if !p.record.Login && p.replaySet && offset < p.replayUntil {
		cut := int(p.replayUntil - offset)
		if cut > len(data) {
			cut = len(data)
		}
		p.chunks = append(p.chunks, shellOutput{data: data[:cut], replay: true})
		data = data[cut:]
		offset += uint64(cut)
	}
	if len(data) > 0 {
		p.chunks = append(p.chunks, shellOutput{data: data, replay: false})
		offset += uint64(len(data))
	}
	if offset > p.offset {
		p.offset = offset
	}
}

func (p *guestShellPTY) takeGap() (terminalGap, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.gapPending {
		return terminalGap{}, false
	}
	p.gapPending = false
	return p.gap, true
}

func (p *guestShellPTY) Write(data []byte) (int, error) {
	const maxChunk = 64 << 10
	sent := 0
	for sent < len(data) {
		n := len(data) - sent
		if n > maxChunk {
			n = maxChunk
		}
		if err := p.writeChunk(data[sent : sent+n]); err != nil {
			if sent == 0 {
				return 0, err
			}
			return sent, errInputUnknown
		}
		sent += n
	}
	return sent, nil
}

func (p *guestShellPTY) writeChunk(data []byte) error {
	if p.stoppedForDetach() {
		return errTerminalDetached
	}
	id := randomToken()[:32]
	p.noteInput(id, "accepted")
	parent := p.life
	if p.killed.Load() {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	var status string
	err := p.roundTrip(ctx, func(conn io.ReadWriteCloser) error {
		var result struct {
			Status string `json:"status"`
		}
		callErr := guestShellCall(conn, gsInput, map[string]any{
			"shell_id": p.record.ShellID, "epoch": p.record.Epoch, "pid": p.record.PID,
			"generation": p.record.Generation, "input_id": id, "data": base64.StdEncoding.EncodeToString(data),
		}, &result)
		status = result.Status
		return callErr
	})
	if p.stoppedForDetach() {
		p.noteInput(id, "unknown")
		return errTerminalDetached
	}
	if err != nil || (status != "written" && status != "") || status == "rejected" {
		p.noteInput(id, "unknown")
		return errInputUnknown
	}
	if status != "written" {
		p.noteInput(id, "unknown")
		return errInputUnknown
	}
	p.noteInput(id, "written")
	return nil
}

func (p *guestShellPTY) noteInput(id, status string) {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	if _, ok := p.inputs[id]; ok {
		p.inputs[id] = status
		return
	}
	if len(p.inputOrder) >= guestInputRemember {
		oldest := p.inputOrder[0]
		p.inputOrder = p.inputOrder[1:]
		delete(p.inputs, oldest)
		if cap(p.inputOrder) > guestInputRemember*2 {
			p.inputOrder = append([]string(nil), p.inputOrder...)
		}
	}
	p.inputOrder = append(p.inputOrder, id)
	p.inputs[id] = status
}

func (p *guestShellPTY) inputStatus(id string) string {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	return p.inputs[id]
}

func (p *guestShellPTY) Resize(cols, rows uint16) error {
	if p.stoppedForDetach() {
		return errTerminalDetached
	}
	seq := p.resizeSeq.Add(1)
	ctx, cancel := context.WithTimeout(p.life, 3*time.Second)
	defer cancel()
	var result struct {
		Seq uint64 `json:"seq"`
	}
	err := p.roundTrip(ctx, func(conn io.ReadWriteCloser) error {
		return guestShellCall(conn, gsResize, map[string]any{
			"shell_id": p.record.ShellID, "epoch": p.record.Epoch, "pid": p.record.PID,
			"generation": p.record.Generation, "cols": cols, "rows": rows, "seq": seq, "writer": p.record.HostID,
		}, &result)
	})
	if err != nil {
		return err
	}
	for {
		current := p.resizeSeq.Load()
		if result.Seq <= current || p.resizeSeq.CompareAndSwap(current, result.Seq) {
			return nil
		}
	}
}

func (p *guestShellPTY) Kill() error {
	p.killed.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	err := p.roundTrip(ctx, func(conn io.ReadWriteCloser) error {
		var result struct {
			Stopped bool `json:"stopped"`
		}
		return guestShellCall(conn, gsStop, map[string]any{
			"shell_id": p.record.ShellID, "epoch": p.record.Epoch, "pid": p.record.PID, "generation": p.record.Generation,
		}, &result)
	})
	if p.profile != "" && (err == nil || errors.Is(err, errTerminalLost)) {
		_ = forgetGuestShell(p.profile, p.record)
	}
	p.closeConn()
	p.finish(err)
	p.fail(io.EOF)
	return err
}

func forgetGuestShell(profile string, rec guestShellRecord) error {
	guestShellMetaMu.Lock()
	defer guestShellMetaMu.Unlock()
	cat, err := loadGuestShellCatalogLocked(profile)
	if err != nil {
		return err
	}
	next := cat.Shells[:0]
	for _, item := range cat.Shells {
		if rec.CreateID != "" && item.CreateID == rec.CreateID {
			continue
		}
		if rec.ShellID != "" && item.ShellID == rec.ShellID {
			continue
		}
		if rec.HostID != "" && item.HostID == rec.HostID {
			continue
		}
		next = append(next, item)
	}
	cat.Shells = next
	return saveGuestShellCatalogLocked(profile, cat)
}

func (p *guestShellPTY) Close() error {
	p.closeConn()
	return nil
}

func (p *guestShellPTY) Wait() error {
	<-p.exited
	return p.waitErr
}

func (p *guestShellPTY) stoppedForDetach() bool {
	return p.detachOnly.Load() || (p.life != nil && p.life.Err() != nil && !p.killed.Load())
}

func (p *guestShellPTY) roundTrip(ctx context.Context, fn func(io.ReadWriteCloser) error) error {
	conn, err := p.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if _, err = guestShellHello(conn, p.token); err != nil {
		return err
	}
	return fn(conn)
}

func (p *guestShellPTY) loop() {
	backoff := 50 * time.Millisecond
	for {
		if p.killed.Load() {
			p.finish(nil)
			p.fail(io.EOF)
			return
		}
		if p.stoppedForDetach() {
			p.fail(errTerminalDetached)
			return
		}
		err := p.attachOnce()
		if p.killed.Load() {
			p.finish(nil)
			p.fail(io.EOF)
			return
		}
		if errors.Is(err, errTerminalLost) {
			p.fail(errTerminalLost)
			return
		}
		if err == nil {
			p.finish(nil)
			p.fail(io.EOF)
			return
		}
		if p.stoppedForDetach() {
			p.fail(errTerminalDetached)
			return
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-p.life.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			p.fail(errTerminalDetached)
			return
		}
		if backoff < time.Second {
			backoff *= 2
		}
	}
}

func (p *guestShellPTY) attachOnce() error {
	ctx := p.life
	conn, err := p.dial(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.conn = conn
	p.mu.Unlock()
	defer p.closeConn()
	if _, err = guestShellHello(conn, p.token); err != nil {
		return err
	}
	if err = writeJSONFrame(conn, gsAttach, map[string]any{
		"shell_id": p.record.ShellID, "epoch": p.record.Epoch, "pid": p.record.PID,
		"generation": p.record.Generation, "offset": p.offset,
	}); err != nil {
		return err
	}
	for {
		if p.killed.Load() {
			return io.EOF
		}
		if p.stoppedForDetach() {
			return errTerminalDetached
		}
		kind, payload, err := readFrame(conn)
		if err != nil {
			return err
		}
		switch kind {
		case gsAttached:
			var attached struct {
				Offset    uint64 `json:"offset"`
				End       uint64 `json:"end"`
				ResizeSeq uint64 `json:"resize_seq"`
			}
			if json.Unmarshal(payload, &attached) == nil {
				if attached.ResizeSeq > 0 {
					for {
						current := p.resizeSeq.Load()
						if attached.ResizeSeq <= current || p.resizeSeq.CompareAndSwap(current, attached.ResizeSeq) {
							break
						}
					}
				}
				p.mu.Lock()
				if !p.replaySet {
					p.replayUntil = attached.End
					p.replaySet = true
				}
				p.mu.Unlock()
			}
		case gsGap:
			var gap struct {
				From    uint64 `json:"from"`
				To      uint64 `json:"to"`
				Omitted bool   `json:"omitted"`
			}
			if json.Unmarshal(payload, &gap) != nil {
				return errors.New("guest shell gap")
			}
			p.mu.Lock()
			p.gap = terminalGap{From: gap.From, To: gap.To, Omitted: gap.Omitted}
			p.gapPending = true
			if !gap.Omitted && gap.To > p.offset {
				p.offset = gap.To
			}
			p.cond.Broadcast()
			p.mu.Unlock()
		case gsOutput:
			if len(payload) < 8 {
				return errors.New("guest shell output")
			}
			offset := binary.BigEndian.Uint64(payload[:8])
			data := append([]byte(nil), payload[8:]...)
			p.mu.Lock()
			if !p.record.Login && offset > p.offset {
				p.gap = terminalGap{From: p.offset, To: offset}
				p.gapPending = true
			}
			p.queueOutputLocked(offset, data)
			p.cond.Broadcast()
			p.mu.Unlock()
		case gsExit:
			return nil
		case gsLost:
			return errTerminalLost
		case gsError:
			return errors.New("客机终端附着失败")
		default:
			return errors.New("guest shell frame")
		}
	}
}

func (p *guestShellPTY) closeConn() {
	p.mu.Lock()
	conn := p.conn
	p.conn = nil
	p.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (p *guestShellPTY) fail(err error) {
	p.mu.Lock()
	if p.fatal == nil {
		p.fatal = err
	}
	p.cond.Broadcast()
	p.mu.Unlock()
}

func (p *guestShellPTY) finish(err error) {
	p.finishOnce.Do(func() {
		p.waitErr = err
		close(p.exited)
	})
}
