package workbench

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

type markerPTY struct {
	marked  atomic.Bool
	kills   atomic.Int32
	release chan struct{}
	killed  chan struct{}
	once    sync.Once
}

func (m *markerPTY) beginDetach() { m.marked.Store(true) }
func (m *markerPTY) detach()      { m.beginDetach() }
func (m *markerPTY) Read(b []byte) (int, error) {
	select {
	case <-m.killed:
		return 0, io.EOF
	case <-m.release:
	}
	if m.marked.Load() {
		return 0, errTerminalDetached
	}
	return 0, io.EOF
}
func (m *markerPTY) Write(b []byte) (int, error) {
	if m.marked.Load() {
		return 0, errTerminalDetached
	}
	return len(b), nil
}
func (m *markerPTY) Resize(uint16, uint16) error { return nil }
func (m *markerPTY) Kill() error {
	m.kills.Add(1)
	m.once.Do(func() { close(m.killed) })
	return nil
}
func (m *markerPTY) Close() error { return nil }
func (m *markerPTY) Wait() error  { return nil }

func TestBeginDetachDoesNotStopGuestShell(t *testing.T) {
	var manager terminalManager
	file := &markerPTY{release: make(chan struct{}), killed: make(chan struct{})}
	session, err := manager.startSessionWithPTY(nil, "/workspace", "shell", claudeSelection{}, nil, func() (terminalPTY, error) {
		return file, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.beginDetach()
	if !manager.closed || !file.marked.Load() || file.kills.Load() != 0 {
		t.Fatal("beginDetach waited, failed to mark, or stopped the shell")
	}
	close(file.release)
	awaitTerminalSignal(t, session.done)
	if file.kills.Load() != 0 || session.summary()["state"] != "detached" {
		t.Fatalf("detached EOF killed the shell: kills=%d summary=%v", file.kills.Load(), session.summary())
	}
}

func TestCloseAllStopsMarkedGuestShell(t *testing.T) {
	var manager terminalManager
	file := &markerPTY{release: make(chan struct{}), killed: make(chan struct{})}
	session, err := manager.startSessionWithPTY(nil, "/workspace", "shell", claudeSelection{}, nil, func() (terminalPTY, error) {
		return file, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.beginDetach()
	manager.closeAll()
	awaitTerminalSignal(t, session.done)
	if file.kills.Load() == 0 {
		t.Fatal("explicit stop did not kill a detach-marked shell")
	}
}

func TestRestoreWithoutRecordsDoesNotDial(t *testing.T) {
	s := testServer(t)
	dialed := false
	setGuestShellDial(s, func(context.Context) (io.ReadWriteCloser, error) {
		dialed = true
		return nil, errors.New("should not dial")
	})
	t.Cleanup(func() { setGuestShellDial(s, nil) })
	if err := s.restoreGuestTerminals(context.Background()); err != nil || dialed {
		t.Fatal(err, dialed)
	}
}

func TestGuestShellCatalogOmitsTerminalBytes(t *testing.T) {
	dir := t.TempDir()
	rec := guestShellRecord{
		CreateID: "0123456789abcdef0123456789abcdef", ShellID: "abcdefabcdefabcd",
		Workspace: "/work", PID: 42, Generation: "100", Kind: "shell",
	}
	if err := upsertGuestShellRecord(dir, rec); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(guestShellCatalogPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, forbidden := range []string{"output", "script", "input", "SECRET"} {
		if containsWord(text, forbidden) {
			t.Fatalf("catalog stored %s: %s", forbidden, text)
		}
	}
	loaded, err := loadGuestShellCatalog(dir)
	if err != nil || len(loaded.Shells) != 1 || loaded.Shells[0].PID != 42 || loaded.Shells[0].ShellID != rec.ShellID {
		t.Fatal(err, loaded)
	}
}

func containsWord(text, word string) bool {
	return len(text) >= len(word) && (text == word || len(text) > 0 && (stringIndex(text, word) >= 0))
}

func stringIndex(text, word string) int {
	for i := 0; i+len(word) <= len(text); i++ {
		if text[i:i+len(word)] == word {
			return i
		}
	}
	return -1
}
