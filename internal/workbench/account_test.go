package workbench

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newTestAccount(t *testing.T) *localAccount {
	t.Helper()
	a, err := loadLocalAccount(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = a.create("林间工作室", "initial-test-password", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.lock)
	return a
}

func TestLocalAccountEncryptionPasswordChangeAndReload(t *testing.T) {
	a := newTestAccount(t)
	key, _ := a.desktopKey()
	defer clear(key)
	data, err := os.ReadFile(a.path)
	if err != nil || bytes.Contains(data, []byte("initial-test-password")) || bytes.Contains(data, key) {
		t.Fatal("plaintext credentials persisted", err)
	}
	if err = a.changePassword("initial-test-password", "replacement-test-password"); err != nil {
		t.Fatal(err)
	}
	a.lock()
	reloaded, err := loadLocalAccount(filepath.Dir(a.path))
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.lock()
	if reloaded.view().Unlocked {
		t.Fatal("reloaded account must be locked")
	}
	if err = reloaded.unlock("initial-test-password"); err == nil {
		t.Fatal("old password accepted")
	}
	reloaded.nextAttempt = time.Time{}
	if err = reloaded.unlock("replacement-test-password"); err != nil {
		t.Fatal(err)
	}
	after, _ := reloaded.desktopKey()
	defer clear(after)
	if !bytes.Equal(after, key) {
		t.Fatal("password change rotated the guest key and lost existing login access")
	}
	if err = reloaded.create("other", "new-password", ""); err == nil {
		t.Fatal("second account replaced the existing account")
	}
}

func TestLocalAccountFailedSavePreservesExistingPassword(t *testing.T) {
	a := newTestAccount(t)
	path := a.path
	a.path = t.TempDir()
	if err := a.changePassword("initial-test-password", "replacement-test-password"); err == nil {
		t.Fatal("save unexpectedly succeeded")
	}
	a.path = path
	a.lock()
	if err := a.unlock("initial-test-password"); err != nil {
		t.Fatal("failed save changed the active password", err)
	}
}

func TestLocalAccountProfileAndAvatarPersistWithoutRekey(t *testing.T) {
	a := newTestAccount(t)
	before, _ := a.desktopKey()
	defer clear(before)
	img := image.NewRGBA(image.Rect(0, 0, 200, 100))
	img.Set(100, 50, color.RGBA{R: 240, G: 100, B: 40, A: 255})
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	avatar := "data:image/png;base64," + base64.StdEncoding.EncodeToString(b.Bytes())
	if err := a.updateProfile("  山雀  ", avatar); err != nil {
		t.Fatal(err)
	}
	data, _ := base64.StdEncoding.DecodeString(strings.SplitN(a.view().Avatar, ",", 2)[1])
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width != 128 || cfg.Height != 128 {
		t.Fatal("avatar was not normalized", cfg, err)
	}
	loaded, err := loadLocalAccount(filepath.Dir(a.path))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.view().Username != "山雀" || loaded.view().Avatar != a.view().Avatar {
		t.Fatal("profile did not persist")
	}
	after, _ := a.desktopKey()
	defer clear(after)
	if !bytes.Equal(before, after) {
		t.Fatal("profile edit changed credential key")
	}
	if err := a.updateProfile("山雀", "data:image/svg+xml;base64,PHN2Zy8+"); err == nil {
		t.Fatal("active content accepted as avatar")
	}
}

func TestLocalAccountCorruptionCannotBecomeFirstRun(t *testing.T) {
	a := newTestAccount(t)
	if err := os.WriteFile(a.path, []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLocalAccount(filepath.Dir(a.path)); err == nil {
		t.Fatal("corrupt account silently became setup/unlocked")
	}
}

func TestAccountLockBlocksRoutesAndCancelsAccess(t *testing.T) {
	s := testServer(t)
	ctx, ok := s.account.access()
	if !ok {
		t.Fatal("fixture locked")
	}
	w := request(s, "POST", "/api/account/lock", s.origin, "{}", true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("active requests not cancelled")
	}
	for _, path := range []string{"/api/settings", "/api/terminals", "/api/vnc-credentials", "/api/claude-desktop/vnc-credentials", "/api/audio"} {
		if w = request(s, "GET", path, "", "", true); w.Code != 423 {
			t.Fatal(path, w.Code)
		}
	}
	if w = request(s, "GET", "/api/account", "", "", true); w.Code != 200 {
		t.Fatal("lock screen cannot read public profile")
	}
	if w = request(s, "GET", "/api/account", "", "", false); w.Code != 401 {
		t.Fatal("account bypassed host session")
	}
}

func TestLockDisconnectsTerminalViewerWithoutEndingSession(t *testing.T) {
	s := testServer(t)
	term := &terminalSession{id: "keep", title: "ongoing work", history: []byte("still running"), clients: map[chan terminalFrame]bool{}}
	s.terminals.sessions = map[string]*terminalSession{term.id: term}
	server := httptest.NewServer(s.handler())
	defer server.Close()
	s.origin = server.URL
	headers := http.Header{"Origin": {server.URL}, "Cookie": {s.cookieName() + "=" + s.session}}
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/terminals/keep/stream", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	var marker struct {
		Type string `json:"type"`
	}
	if err := ws.ReadJSON(&marker); err != nil || marker.Type != "history" {
		t.Fatalf("history marker: %+v, %v", marker, err)
	}
	_, data, err := ws.ReadMessage()
	if err != nil || string(data) != "still running" {
		t.Fatal("viewer did not attach", err)
	}
	s.account.lock()
	_ = ws.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err = ws.ReadMessage(); err == nil {
		t.Fatal("locked viewer retained its connection")
	}
	term.mu.Lock()
	defer term.mu.Unlock()
	if term.closed || string(term.history) != "still running" {
		t.Fatal("locking destroyed the background terminal")
	}
}

func TestLoginRequiresValidPasswordBeforeProtectedAPIs(t *testing.T) {
	s := testServer(t)
	a := newTestAccount(t)
	a.lock()
	s.account = a
	w := request(s, "POST", "/api/account/unlock", s.origin, `{"password":"wrong-password"}`, true)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w = request(s, "GET", "/api/settings", "", "", true); w.Code != 423 {
		t.Fatal("wrong password granted access")
	}
	a.nextAttempt = time.Time{}
	w = request(s, "POST", "/api/account/unlock", s.origin, `{"password":"initial-test-password"}`, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = request(s, "GET", "/api/settings", "", "", true); w.Code != 200 {
		t.Fatal(w.Code)
	}
	ctx, ok := a.access()
	if !ok || ctx.Err() != nil {
		t.Fatal("unlock did not establish fresh access context")
	}
}

func TestLogoutJoinsActiveStopAndLocksOnlyOnSuccess(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failed], func(t *testing.T) {
			s := testServer(t)
			started, finish := make(chan struct{}), make(chan struct{})
			s.launch(httptest.NewRecorder(), "stop", func() error {
				close(started)
				<-finish
				if failed {
					return errors.New("stop failed")
				}
				return nil
			})
			<-started
			w := request(s, "POST", "/api/account/logout", s.origin, "{}", true)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			if !s.account.view().Unlocked {
				t.Fatal("logout locked before stop completed")
			}
			close(finish)
			s.op.Lock()
			defer s.op.Unlock()
			if s.account.view().Unlocked != failed {
				t.Fatal("logout did not follow the active stop result")
			}
		})
	}
}
