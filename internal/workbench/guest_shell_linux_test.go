//go:build linux

package workbench

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func startGuestShellDaemon(t *testing.T, ring int) (string, string, func()) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required")
	}
	dir, err := os.MkdirTemp("/tmp", "cage-gs")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s")
	tokenPath := filepath.Join(dir, "token")
	token := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err = os.WriteFile(tokenPath, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-I", "guest_shell.py", "serve")
	cmd.Env = append(os.Environ(),
		"CAGE_GUEST_SHELL_SOCK="+sock,
		"CAGE_GUEST_SHELL_TOKEN_FILE="+tokenPath,
		"CAGE_GUEST_SHELL_RING="+strconv.Itoa(ring),
	)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.Dial("unix", sock)
		if dialErr == nil {
			_ = conn.Close()
			return sock, token, func() {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
				_ = os.RemoveAll(dir)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	t.Fatal("daemon socket did not appear")
	return "", "", func() {}
}

func daemonConn(t *testing.T, sock, token string) (net.Conn, string) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := guestShellHello(conn, token)
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	return conn, epoch
}

func daemonCall(t *testing.T, conn net.Conn, kind byte, value any, out any) {
	t.Helper()
	if err := guestShellCall(conn, kind, value, out); err != nil {
		t.Fatal(err)
	}
}

func waitShellState(t *testing.T, conn net.Conn, id, state string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var listed struct {
			Shells []guestShellInfo `json:"shells"`
		}
		daemonCall(t, conn, gsList, map[string]any{}, &listed)
		for _, item := range listed.Shells {
			if item.ShellID == id && item.State == state {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("shell %s did not reach %s", id, state)
}

func TestGuestDaemonCreateSurvivesDisconnectAndIsIdempotent(t *testing.T) {
	sock, token, stop := startGuestShellDaemon(t, 4096)
	defer stop()
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	conn, epoch := daemonConn(t, sock, token)
	defer conn.Close()
	var created guestShellInfo
	createID := "0123456789abcdef0123456789abcdef"
	script := "printf x >> '" + count + "'; exec sleep 60"
	daemonCall(t, conn, gsCreate, map[string]any{"create_id": createID, "script": script, "cwd": dir, "cols": 80, "rows": 24}, &created)
	if created.PID <= 0 || created.Epoch != epoch || created.Generation == "" {
		t.Fatalf("missing process proof: %+v", created)
	}
	_ = conn.Close()
	if _, err := os.Stat("/proc/" + strconv.Itoa(created.PID)); err != nil {
		t.Fatal("shell exited when the client disconnected", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var data []byte
	var err error
	for time.Now().Before(deadline) {
		data, err = os.ReadFile(count)
		if err == nil && string(data) == "x" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if string(data) != "x" {
		t.Fatalf("shell did not record its start before the duplicate create: %q %v", data, err)
	}
	again, epoch2 := daemonConn(t, sock, token)
	defer again.Close()
	var second guestShellInfo
	daemonCall(t, again, gsCreate, map[string]any{"create_id": createID, "script": script, "cwd": dir, "cols": 80, "rows": 24}, &second)
	data, err = os.ReadFile(count)
	if err != nil || string(data) != "x" || second.PID != created.PID || second.ShellID != created.ShellID || epoch2 != epoch {
		t.Fatalf("command ran twice or identity changed: %q %+v %v", data, second, err)
	}
	var stopped struct {
		Stopped bool `json:"stopped"`
	}
	request := map[string]any{"shell_id": created.ShellID, "epoch": epoch, "pid": created.PID, "generation": created.Generation}
	daemonCall(t, again, gsStop, request, &stopped)
	daemonCall(t, again, gsStop, request, &stopped)
	if !stopped.Stopped {
		t.Fatal("stop was not idempotent")
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err = os.Stat("/proc/" + strconv.Itoa(created.PID)); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("stop did not reap the shell")
}

func TestGuestDaemonShortLivedOutputRemainsAttachable(t *testing.T) {
	sock, token, stop := startGuestShellDaemon(t, 4096)
	defer stop()
	dir := t.TempDir()
	conn, epoch := daemonConn(t, sock, token)
	defer conn.Close()
	var created guestShellInfo
	daemonCall(t, conn, gsCreate, map[string]any{
		"create_id": "44444444444444444444444444444444",
		"script":    "printf 'short-lived-marker\\n'; exit 0",
		"cwd":       dir, "cols": 80, "rows": 24,
	}, &created)
	waitShellState(t, conn, created.ShellID, "exited")
	attach, _ := daemonConn(t, sock, token)
	defer attach.Close()
	if err := writeJSONFrame(attach, gsAttach, map[string]any{
		"shell_id": created.ShellID, "epoch": epoch, "pid": created.PID, "generation": created.Generation, "offset": 0,
	}); err != nil {
		t.Fatal(err)
	}
	_ = attach.SetReadDeadline(time.Now().Add(3 * time.Second))
	var output []byte
	for {
		kind, payload, err := readFrame(attach)
		if err != nil {
			t.Fatal(err)
		}
		if kind == gsOutput && len(payload) >= 8 {
			output = append(output, payload[8:]...)
		}
		if kind == gsExit {
			if !bytes.Contains(output, []byte("short-lived-marker")) {
				t.Fatalf("exited shell dropped its output before attach: %q", output)
			}
			return
		}
	}
}

func TestGuestDaemonRingGapDoesNotStallAndLoginIsOmitted(t *testing.T) {
	sock, token, stop := startGuestShellDaemon(t, 1024)
	defer stop()
	dir := t.TempDir()
	conn, epoch := daemonConn(t, sock, token)
	defer conn.Close()
	var created guestShellInfo
	daemonCall(t, conn, gsCreate, map[string]any{
		"create_id": "abcdefabcdefabcdefabcdefabcdefab",
		"script":    "python3 -c 'import sys; sys.stdout.write(\"A\"*9000+\"END\")'",
		"cwd":       dir, "cols": 40, "rows": 10,
	}, &created)
	waitShellState(t, conn, created.ShellID, "exited")
	attach, _ := daemonConn(t, sock, token)
	defer attach.Close()
	if err := writeJSONFrame(attach, gsAttach, map[string]any{
		"shell_id": created.ShellID, "epoch": epoch, "pid": created.PID, "generation": created.Generation, "offset": 0,
	}); err != nil {
		t.Fatal(err)
	}
	var sawGap bool
	var output []byte
	deadline := time.Now().Add(3 * time.Second)
	_ = attach.SetReadDeadline(deadline)
	for {
		kind, payload, err := readFrame(attach)
		if err != nil {
			t.Fatal(err)
		}
		switch kind {
		case gsGap:
			sawGap = true
		case gsOutput:
			if len(payload) >= 8 {
				output = append(output, payload[8:]...)
			}
		case gsExit:
			if !sawGap || !bytes.Contains(output, []byte("END")) || len(output) >= 9000 {
				t.Fatalf("gap=%v len=%d tail=%q", sawGap, len(output), tail(output))
			}
			goto login
		}
	}
login:
	secret := "SECRET_LOGIN_OUTPUT"
	var sensitive guestShellInfo
	daemonCall(t, conn, gsCreate, map[string]any{
		"create_id": "fedcba9876543210fedcba9876543210", "script": "printf '" + secret + "'; exit 0",
		"cwd": "", "login": true, "cols": 80, "rows": 24,
	}, &sensitive)
	waitShellState(t, conn, sensitive.ShellID, "exited")
	late, _ := daemonConn(t, sock, token)
	defer late.Close()
	if err := writeJSONFrame(late, gsAttach, map[string]any{
		"shell_id": sensitive.ShellID, "epoch": sensitive.Epoch, "pid": sensitive.PID, "generation": sensitive.Generation, "offset": 0,
	}); err != nil {
		t.Fatal(err)
	}
	_ = late.SetReadDeadline(time.Now().Add(3 * time.Second))
	var body []byte
	for {
		kind, payload, err := readFrame(late)
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, payload...)
		if kind == gsExit {
			break
		}
	}
	if bytes.Contains(body, []byte(secret)) {
		t.Fatal("login history was retained")
	}
	root := filepath.Dir(sock)
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		data, _ := os.ReadFile(filepath.Join(root, entry.Name()))
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("login output was stored on disk")
		}
	}
}

func TestGuestDaemonResizeEpochAndInputAreSingleUse(t *testing.T) {
	sock, token, stop := startGuestShellDaemon(t, 4096)
	defer stop()
	dir := t.TempDir()
	out := filepath.Join(dir, "lines")
	conn, epoch := daemonConn(t, sock, token)
	defer conn.Close()
	var created guestShellInfo
	daemonCall(t, conn, gsCreate, map[string]any{
		"create_id": "00112233445566778899aabbccddeeff",
		"script":    "while IFS= read -r line; do printf '%s\\n' \"$line\" >> '" + out + "'; done",
		"cwd":       dir, "cols": 80, "rows": 24,
	}, &created)
	var resized struct {
		Cols int `json:"cols"`
		Seq  int `json:"seq"`
	}
	daemonCall(t, conn, gsResize, map[string]any{
		"shell_id": created.ShellID, "epoch": epoch, "pid": created.PID, "generation": created.Generation,
		"cols": 40, "rows": 12, "seq": 2, "writer": "new",
	}, &resized)
	daemonCall(t, conn, gsResize, map[string]any{
		"shell_id": created.ShellID, "epoch": epoch, "pid": created.PID, "generation": created.Generation,
		"cols": 10, "rows": 10, "seq": 1, "writer": "old",
	}, &resized)
	if resized.Cols != 40 || resized.Seq != 2 {
		t.Fatalf("stale resize won: %+v", resized)
	}
	var input struct {
		Status string `json:"status"`
	}
	body := map[string]any{
		"shell_id": created.ShellID, "epoch": epoch, "pid": created.PID, "generation": created.Generation,
		"input_id": "abcabcabcabcabcabcabcabcabcabcab", "data": "b25lCg==",
	}
	daemonCall(t, conn, gsInput, body, &input)
	if input.Status != "written" {
		t.Fatal(input.Status)
	}
	daemonCall(t, conn, gsInput, body, &input)
	deadline := time.Now().Add(2 * time.Second)
	var data []byte
	var err error
	for time.Now().Before(deadline) {
		data, err = os.ReadFile(out)
		if err == nil && string(data) == "one\n" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if string(data) != "one\n" {
		t.Fatalf("input was duplicated or dropped: %q %v", data, err)
	}
	lost, _ := daemonConn(t, sock, token)
	defer lost.Close()
	if err = writeJSONFrame(lost, gsAttach, map[string]any{
		"shell_id": created.ShellID, "epoch": "not-the-epoch", "pid": created.PID, "generation": created.Generation, "offset": 0,
	}); err != nil {
		t.Fatal(err)
	}
	kind, _, err := readFrame(lost)
	if err != nil || kind != gsLost {
		t.Fatalf("old epoch attached: %d %v", kind, err)
	}
	if _, err = os.Stat("/proc/" + strconv.Itoa(created.PID)); err != nil {
		t.Fatal("rejected attach killed the shell")
	}
	badGen, _ := daemonConn(t, sock, token)
	defer badGen.Close()
	if err = writeJSONFrame(badGen, gsAttach, map[string]any{
		"shell_id": created.ShellID, "epoch": epoch, "pid": created.PID, "generation": "1", "offset": 0,
	}); err != nil {
		t.Fatal(err)
	}
	kind, _, err = readFrame(badGen)
	if err != nil || kind != gsLost {
		t.Fatalf("wrong generation attached: %d %v", kind, err)
	}
}

func TestGuestShellReattachKeepsProcess(t *testing.T) {
	sock, token, stop := startGuestShellDaemon(t, 4096)
	defer stop()
	dir := t.TempDir()
	conn, _ := daemonConn(t, sock, token)
	defer conn.Close()
	var created guestShellInfo
	daemonCall(t, conn, gsCreate, map[string]any{
		"create_id": "22222222222222222222222222222222",
		"script":    "printf 'ready\\n'; exec cat",
		"cwd":       dir, "cols": 80, "rows": 24,
	}, &created)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pty := newGuestShellPTY(func(context.Context) (io.ReadWriteCloser, error) {
		return net.Dial("unix", sock)
	}, token, guestShellRecord{
		ShellID: created.ShellID, Epoch: created.Epoch, PID: created.PID, Generation: created.Generation, Cols: 80, Rows: 24,
	}, ctx)
	t.Cleanup(func() { _ = pty.Kill() })
	waitPTY(t, pty, "ready")
	pty.closeConn()
	if _, err := os.Stat("/proc/" + strconv.Itoa(created.PID)); err != nil {
		t.Fatal("closing the attachment killed the shell", err)
	}
	if _, err := pty.Write([]byte("more\n")); err != nil {
		t.Fatal(err)
	}
	waitPTY(t, pty, "more")
	if err := pty.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := pty.Kill(); err != nil {
		t.Fatal("second stop failed", err)
	}
}

func waitPTY(t *testing.T, pty *guestShellPTY, needle string) {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 128)
		var text string
		for !strings.Contains(text, needle) {
			n, err := pty.Read(buf)
			if n > 0 {
				text += string(buf[:n])
			}
			if err != nil && err != errTerminalGap {
				break
			}
		}
		done <- text
	}()
	select {
	case text := <-done:
		if !strings.Contains(text, needle) {
			t.Fatalf("missing %s in %q", needle, text)
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("timed out waiting for %s", needle)
	}
}

func tail(data []byte) string {
	if len(data) > 32 {
		data = data[len(data)-32:]
	}
	return string(data)
}

type dropFrameConn struct {
	net.Conn
	raw, pending []byte
	frames       int
	dropAt       int
	outbound     []byte
	inputDrop    *atomic.Bool
}

func (c *dropFrameConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if c.inputDrop == nil {
		return n, err
	}
	c.outbound = append(c.outbound, p[:n]...)
	for len(c.outbound) >= 4 {
		size := int(binary.BigEndian.Uint32(c.outbound[:4]))
		if size < 1 || size > guestShellMax || len(c.outbound) < 4+size {
			break
		}
		if c.outbound[4] == gsInput && c.inputDrop.CompareAndSwap(false, true) {
			// The hello reply has already been consumed. Drop the input reply,
			// never an output attachment that happened to dial first.
			c.dropAt = c.frames + 1
		}
		c.outbound = c.outbound[4+size:]
	}
	return n, err
}

func (c *dropFrameConn) Read(p []byte) (int, error) {
	for {
		if len(c.pending) > 0 {
			n := copy(p, c.pending)
			c.pending = c.pending[n:]
			return n, nil
		}
		if c.frames >= c.dropAt {
			return 0, io.EOF
		}
		buf := make([]byte, 1024)
		n, err := c.Conn.Read(buf)
		if n > 0 {
			c.raw = append(c.raw, buf[:n]...)
			c.peel()
		}
		if len(c.pending) > 0 {
			continue
		}
		if err != nil {
			return 0, err
		}
	}
}

func (c *dropFrameConn) peel() {
	for len(c.raw) >= 4 && c.frames < c.dropAt {
		size := int(binary.BigEndian.Uint32(c.raw[:4]))
		if size < 1 || size > guestShellMax || len(c.raw) < 4+size {
			return
		}
		if c.frames+1 >= c.dropAt {
			c.frames = c.dropAt
			c.raw = nil
			_ = c.Conn.Close()
			return
		}
		c.pending = append(c.pending, c.raw[:4+size]...)
		c.raw = c.raw[4+size:]
		c.frames++
	}
}

func TestGuestInputDisconnectIsNotReplayed(t *testing.T) {
	sock, token, stop := startGuestShellDaemon(t, 4096)
	defer stop()
	dir := t.TempDir()
	out := filepath.Join(dir, "lines")
	conn, _ := daemonConn(t, sock, token)
	defer conn.Close()
	var created guestShellInfo
	daemonCall(t, conn, gsCreate, map[string]any{
		"create_id": "33333333333333333333333333333333",
		"script":    "while IFS= read -r line; do printf '%s\\n' \"$line\" >> '" + out + "'; done",
		"cwd":       dir, "cols": 80, "rows": 24,
	}, &created)
	var dropped atomic.Bool
	pty := newGuestShellPTY(func(context.Context) (io.ReadWriteCloser, error) {
		raw, err := net.Dial("unix", sock)
		if err != nil {
			return nil, err
		}
		return &dropFrameConn{Conn: raw, dropAt: int(^uint(0) >> 1), inputDrop: &dropped}, nil
	}, token, guestShellRecord{
		ShellID: created.ShellID, Epoch: created.Epoch, PID: created.PID, Generation: created.Generation,
	}, context.Background())
	t.Cleanup(func() { _ = pty.Kill() })
	if _, err := pty.Write([]byte("once\n")); !errors.Is(err, errInputUnknown) {
		t.Fatalf("lost input acknowledgement was treated as success: %v", err)
	}
	if !dropped.Load() {
		t.Fatal("input acknowledgement fault was not exercised")
	}
	if _, err := pty.Write([]byte("next\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(out)
		if strings.Contains(string(data), "next") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	text := string(data)
	if strings.Count(text, "once") != 1 || !strings.Contains(text, "next") {
		t.Fatalf("disconnected input was replayed or the next input was lost: %q", text)
	}
}

func TestRejectedGuestCreatesDoNotConsumeShellIDs(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required")
	}
	// Fill the real deduplication budget with retired requests. A rejected
	// create must not launch a command or retain a newly generated shell ID.
	script := `import os, pathlib, runpy, sys
module = runpy.run_path("guest_shell.py", run_name="cage_shell_test")
service = module["Service"]("0" * 64)
for i in range(1280):
    assert service._remember({"create_id": format(i, "032x"), "shell_id": format(i, "032x"), "state": "exited"})
root = pathlib.Path(sys.argv[1])
marker = root / "must-not-run"
for i in range(1280, 2280):
    try:
        service.create({"create_id": format(i, "032x"), "script": "touch '" + str(marker) + "'", "cwd": str(root), "cols": 80, "rows": 24})
    except ValueError as error:
        assert str(error) == "retain"
    else:
        raise AssertionError("creation was admitted after its deduplication budget was exhausted")
assert not marker.exists(), "rejected creation executed a command"
assert not service.ids, "rejected creation retained generated shell identities"
assert not service.shells, "rejected creation retained a process"
`
	command := exec.Command("python3", "-I", "-c", script, t.TempDir())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("rejected creation was not bounded: %v\n%s", err, output)
	}
}
