package workbench

import (
	"encoding/json"
	"github.com/sylvinus/agent-vm/internal/cage"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"

	"os"
	"path/filepath"
	"testing"

	"github.com/lima-vm/sshocker/pkg/reversesshfs"
	"github.com/pkg/sftp"
)

func TestProjectSelectionKeepsOneVMAndExistingSessionsOffline(t *testing.T) {
	s := testServer(t)
	cfg, _ := loadConfig(s.configPath)
	s.preferences.Workspace = s.environmentDir()
	s.ready, s.networkPaused = true, true
	marker := filepath.Join(s.profile, "state", "lima", cage.InstanceName(s.profile, s.environmentDir()), "cage-running.json")
	if err := writePrivate(marker, map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	originalVM := cage.InstanceName(s.profile, s.environmentDir())
	terminal := &terminalSession{id: "keep", title: "existing terminal"}
	s.terminals.sessions = map[string]*terminalSession{terminal.id: terminal}
	a, _ := filepath.EvalSymlinks(t.TempDir())
	b, _ := filepath.EvalSymlinks(t.TempDir())
	s.projectShares = map[string]*projectShare{a: {done: make(chan struct{})}, b: {done: make(chan struct{})}}
	for _, dir := range []string{a, b, a} {
		next := s.preferences
		next.Workspace = dir
		// The configured proxy is unavailable. Project selection must not probe it.
		if err := s.apply(savedProfile{cfg, next}, true); err != nil {
			t.Fatal(err)
		}
		if !s.ready || !s.vmRunning() || !s.networkPaused || cage.InstanceName(s.profile, s.environmentDir()) != originalVM {
			t.Fatal("project selection changed environment lifecycle or networking")
		}
		if s.terminals.get(terminal.id) != terminal || terminal.closed {
			t.Fatal("project selection interrupted the old terminal")
		}
	}
	var saved sharedProjects
	data, err := os.ReadFile(filepath.Join(s.profile, "projects.json"))
	if err != nil || json.Unmarshal(data, &saved) != nil || len(saved.Paths) != 2 {
		t.Fatal("explicitly opened projects were not persisted uniquely")
	}
	next := s.preferences
	next.Workspace = s.profile
	if err := s.apply(savedProfile{cfg, next}, true); err == nil {
		t.Fatal("private directory accepted as a project")
	}
	if s.workspace() != a || terminal.closed || !s.ready {
		t.Fatal("failed project selection changed the active environment")
	}
}

func TestLiveProjectTransportConfinesReadsAndAllowsGitWrites(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "secret")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(parent, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	server, conn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = reversesshfs.ServeConfined(server, root, false, nil)
	}()
	client, err := sftp.NewClientPipe(conn, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(); _ = conn.Close(); <-done }()
	file, err := client.Create(filepath.Join(root, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("shared")); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	data, err := os.ReadFile(filepath.Join(root, "note.txt"))
	if err != nil || string(data) != "shared" {
		t.Fatal("project write did not reach the host")
	}
	for _, p := range []string{outside, filepath.Join(root, "escape", "secret")} {
		f, err := client.Open(p)
		if err == nil {
			_ = f.Close()
			t.Fatalf("read escaped selected project: %s", p)
		}
	}
	if err := exerciseProjectGitWrites(client, filepath.ToSlash(root)); err != nil {
		t.Fatal(err)
	}
	f, err := client.Open(filepath.Join(root, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil || string(got) != "shared" {
		t.Fatal("shared file read failed")
	}
}

// Git writes temporary objects and lock files, then renames or removes them.
// Exercise these through SFTP rather than host-side filesystem writes.
func exerciseProjectGitWrites(client *sftp.Client, root string) error {
	for _, dir := range []string{".git/objects/ab", ".git/refs/heads", ".git/logs/refs/heads", ".git/hooks", ".vscode", ".claude", ".hg/store"} {
		if err := client.MkdirAll(root + "/" + dir); err != nil {
			return err
		}
	}
	for _, name := range []string{".git/config", ".git/index", ".git/HEAD", ".git/refs/heads/main", ".git/logs/refs/heads/main", ".git/objects/ab/object", ".git/hooks/pre-commit", ".env", ".vscode/settings.json", ".claude/settings.json", ".hg/store/data", "private.key"} {
		file, err := client.Create(root + "/" + name + ".lock")
		if err != nil {
			return err
		}
		_, err = file.Write([]byte("git-write"))
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = client.PosixRename(root+"/"+name+".lock", root+"/"+name); err != nil {
			return err
		}
	}
	return client.Remove(root + "/.git/objects/ab/object")
}

func TestProjectListComesFromSavedDirectories(t *testing.T) {
	s := testServer(t)
	var expected []string
	expected = append(expected, s.environmentDir())
	for i := 0; i < 15; i++ {
		path := t.TempDir()
		if err := s.rememberSharedProject(path); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, path)
	}
	s.preferences.Workspace = expected[2]
	if err := s.rememberSharedProject(expected[2]); err != nil {
		t.Fatal(err)
	}
	// Reload from the same profile, without any browser-local history.
	reloaded := testServer(t)
	reloaded.profile = s.profile
	reloaded.preferences = s.preferences
	w := request(reloaded, "GET", "/api/status", "", "", true)
	var status struct {
		Projects []string `json:"projects"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil || !slices.Equal(status.Projects, expected) {
		t.Fatalf("saved project list was truncated, reordered, or lost: %s", w.Body.String())
	}
}

func TestRestoreProjectSharesReportsTheFailedDirectoryAndReason(t *testing.T) {
	s := testServer(t)
	connected, _ := filepath.EvalSymlinks(t.TempDir())
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Dir(executable)
	s.preferences.Workspace = connected
	s.projectShares = map[string]*projectShare{connected: {done: make(chan struct{})}}
	for _, path := range []string{blocked, connected} {
		if err := s.rememberSharedProject(path); err != nil {
			t.Fatal(err)
		}
	}
	s.restoreProjectShares()
	w := request(s, "GET", "/api/status", "", "", true)
	var status struct {
		ProjectError  string              `json:"project_error"`
		ProjectErrors []projectShareError `json:"project_errors"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil {
		t.Fatalf("cannot read project status: %s", w.Body.String())
	}
	if status.ProjectError == "" || len(status.ProjectErrors) != 1 || status.ProjectErrors[0].Path != blocked || !strings.Contains(status.ProjectErrors[0].Message, "正在运行的工作台程序") {
		t.Fatalf("failure did not identify the blocked project and executable protection: %+v", status)
	}
	if s.projectShares[connected] == nil {
		t.Fatal("one failed directory disconnected a healthy project")
	}
}

func TestProjectReconnectClearsOnlyItsOwnWarning(t *testing.T) {
	s := testServer(t)
	connected, _ := filepath.EvalSymlinks(t.TempDir())
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	missing := filepath.Join(parent, "missing")
	s.projectErrors = []projectShareError{
		{Path: connected, Message: "previous connection failed"},
		{Path: missing, Message: "directory unavailable"},
	}
	s.projectShares = map[string]*projectShare{connected: {done: make(chan struct{})}}
	if err := s.ensureProjectShare(missing); err == nil || len(s.projectErrors) != 2 {
		t.Fatal("a failed reconnect cleared a warning")
	}
	if err := s.ensureProjectShare(connected); err != nil {
		t.Fatal(err)
	}
	if len(s.projectErrors) != 1 || s.projectErrors[0].Path != missing {
		t.Fatalf("reconnect did not preserve the other directory's warning: %+v", s.projectErrors)
	}
	if err := os.Mkdir(missing, 0700); err != nil {
		t.Fatal(err)
	}
	s.projectShares[missing] = &projectShare{done: make(chan struct{})}
	if err := s.ensureProjectShare(missing); err != nil || len(s.projectErrors) != 0 {
		t.Fatalf("successful reconnect left a stale warning: %v, %+v", err, s.projectErrors)
	}
}

func TestProjectActionsUseExplicitDirectoryWithoutSwitchingProject(t *testing.T) {
	s := testServer(t)
	a, _ := filepath.EvalSymlinks(t.TempDir())
	b, _ := filepath.EvalSymlinks(t.TempDir())
	s.preferences.Workspace = b
	s.ready = true
	if err := s.rememberSharedProject(a); err != nil {
		t.Fatal(err)
	}
	s.projectShares = map[string]*projectShare{a: {done: make(chan struct{})}}
	marker := filepath.Join(s.profile, "state", "lima", cage.InstanceName(s.profile, s.environmentDir()), "cage-running.json")
	if err := writePrivate(marker, map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	// The absent SSH config makes this local child exit before connecting.
	body, _ := json.Marshal(map[string]string{"workspace": a})
	w := request(s, "POST", "/api/terminals", s.origin, string(body), true)
	// This fixture has no guest shell service. Creating a terminal must still
	// authorize the explicit directory before any guest command, and must not
	// switch the selected project when the guest command cannot start.
	if w.Code == http.StatusOK || s.workspace() != b {
		t.Fatalf("terminal create without a guest shell changed the project or reported success: %d %s", w.Code, w.Body.String())
	}
	var editorWorkspace string
	s.browserControl = browserClientForTest(t, s, browserTransport(func(r *http.Request) (*http.Response, error) {
		var input struct {
			Workspace string `json:"workspace"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		editorWorkspace = input.Workspace
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"editor"}`)), Header: http.Header{}}, nil
	}))
	for _, action := range []string{"editor", "editor-restart"} {
		w = request(s, "POST", "/api/browser/"+action, s.origin, string(body), true)
		if w.Code != 200 || editorWorkspace != guestPath(a) || s.workspace() != b {
			t.Fatalf("%s changed the selected project or ignored its directory: %s", action, w.Body.String())
		}
	}
	for _, path := range []string{t.TempDir(), s.profile} {
		body, _ = json.Marshal(map[string]string{"workspace": path})
		for _, endpoint := range []string{"/api/terminals", "/api/browser/editor", "/api/browser/editor-restart"} {
			w = request(s, "POST", endpoint, s.origin, string(body), true)
			if w.Code != 400 {
				t.Fatalf("unopened directory accepted by %s: %d", endpoint, w.Code)
			}
		}
	}
	if len(s.terminals.snapshot()) != 0 || s.workspace() != b {
		t.Fatal("rejected requests changed the selected project or left a session")
	}
}
