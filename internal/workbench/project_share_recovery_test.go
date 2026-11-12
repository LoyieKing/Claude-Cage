package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lima-vm/sshocker/pkg/reversesshfs"
)

func TestReplacedProjectRootDoesNotDisturbAnotherShare(t *testing.T) {
	s := testServer(t)
	s.ready = true
	other, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	live := &projectShare{done: make(chan struct{})}
	s.projectShares = map[string]*projectShare{other: live}
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.rememberSharedProject(path); err != nil {
		t.Fatal(err)
	}
	before, err := readProjectRootID(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.confirmProjectRoot(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(path, path+"-replaced"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	after, err := readProjectRootID(path)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("replacement kept the same directory identity")
	}
	if err = s.ensureProjectShare(path); !errors.Is(err, errProjectRootReplaced) {
		t.Fatalf("replaced root admitted: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.projectShares[other] != live || !live.liveLocked() {
		t.Fatal("another project's share changed")
	}
	blocked := s.projectShares[path]
	if blocked == nil || blocked.state != projectShareBlocked {
		t.Fatalf("replaced root state: %+v", blocked)
	}
	if !s.ready {
		t.Fatal("share rejection cleared workbench readiness")
	}
}

func TestRemovedGrantIsNotRestoredByRetry(t *testing.T) {
	s := testServer(t)
	previous := projectShareBackoff
	projectShareBackoff = func(int) time.Duration { return time.Hour }
	t.Cleanup(func() { projectShareBackoff = previous })
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.rememberSharedProject(path); err != nil {
		t.Fatal(err)
	}
	if err = s.ensureProjectShare(path); err == nil {
		t.Fatal("share became ready without a guest")
	}
	s.mu.Lock()
	share := s.projectShares[path]
	s.mu.Unlock()
	if share == nil || share.done == nil {
		t.Fatal("failed share has no watcher")
	}
	if err = s.removeSavedProjectPath(path); err != nil {
		t.Fatal(err)
	}
	if sealed := s.sealProjectShare(path); sealed != share {
		t.Fatal("seal lost the running share")
	}
	select {
	case <-share.done:
	case <-time.After(2 * time.Second):
		t.Fatal("revoked watcher kept running")
	}
	s.recoverProjectShares(context.Background())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.projectShares[path] != nil {
		t.Fatal("removed grant was published again")
	}
}

func TestRecoverSkipsClosingEnvironmentAndClientMounts(t *testing.T) {
	s := testServer(t)
	s.ready = true
	if err := os.MkdirAll(s.environmentDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureProjectShare(s.environmentDir()); err != nil {
		t.Fatal(err)
	}
	if s.projectShares != nil {
		t.Fatal("default environment started a project share manager")
	}
	ctx, cancel := context.WithCancel(context.Background())
	mount := &guestClientMount{ctx: ctx, cancel: cancel, done: make(chan struct{}), ready: true}
	clientPath := clientMountRoot + "/" + strings.Repeat("ab", 16)
	s.clientMounts = map[string]*guestClientMount{clientPath: mount}
	cancel()
	delete(s.clientMounts, clientPath)
	close(mount.done)
	s.recoverProjectShares(context.Background())
	if len(s.clientMounts) != 0 || s.projectShares[clientPath] != nil {
		t.Fatal("client mount was authorized again")
	}
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.rememberSharedProject(path); err != nil {
		t.Fatal(err)
	}
	s.recoverProjectShares(context.Background())
	if err = s.ensureLinuxProjectShare(path); !errors.Is(err, errJobsClosed) {
		t.Fatalf("shutdown admitted a new share: %v", err)
	}
	if !s.ready || s.projectShares[path] != nil {
		t.Fatal("shutdown changed readiness or published a share")
	}
}

func TestContainerDefaultWorkspaceStaysClosedWhenShareDown(t *testing.T) {
	s := testServer(t)
	s.ready = true
	config := []byte(`{"engine":"docker","executable":"/usr/bin/docker","socket":"unix:///tmp/cage-missing.sock","image":"sha256:` + strings.Repeat("a", 64) + `","cpus":2,"memory_bytes":4294967296}`)
	if err := os.WriteFile(filepath.Join(s.profile, "container.json"), config, 0600); err != nil {
		t.Fatal(err)
	}
	err := s.ensureProjectShare(s.environmentDir())
	if err == nil || !s.ready || s.projectShares != nil {
		t.Fatalf("default share admitted work or changed readiness: %v ready=%v shares=%v", err, s.ready, s.projectShares)
	}
}

func TestDegradedShareRejectsNewWork(t *testing.T) {
	s := testServer(t)
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.rememberSharedProject(path); err != nil {
		t.Fatal(err)
	}
	s.projectShares = map[string]*projectShare{path: {path: path, linuxManaged: true, admit: true, state: projectShareDegraded, errText: "共享已断开", done: make(chan struct{})}}
	if err = s.ensureProjectShare(path); err == nil || !strings.Contains(err.Error(), "共享已断开") {
		t.Fatalf("degraded share admitted new work: %v", err)
	}
}

func TestProjectShareCloseDoesNotHangOnBlockedTransport(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	share := &projectShare{transport: projectTransport{ReadCloser: left, WriteCloser: left}, done: make(chan struct{})}
	root := t.TempDir()
	go func() {
		defer close(share.done)
		_ = reversesshfs.ServeConfined(share.transport, root, false, nil)
	}()
	finished := make(chan struct{})
	go func() {
		share.close()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("close stayed blocked behind the share transport")
	}
}

func TestProjectShareStatusReportsIndependentState(t *testing.T) {
	s := testServer(t)
	s.projectShares = map[string]*projectShare{
		"/projects/a": {state: projectShareReady, epoch: 3, done: make(chan struct{})},
		"/projects/b": {state: projectShareDegraded, errText: "暂时失败", epoch: 4, done: make(chan struct{})},
	}
	s.projectErrors = []projectShareError{{Path: "/projects/b", Message: "暂时失败"}, {Path: "/projects/c", Message: "尚未连接"}}
	response := request(s, http.MethodGet, "/api/status", "", "", true)
	var status struct {
		ProjectErrors []projectShareError `json:"project_errors"`
		States        []projectShareState `json:"project_share_states"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &status) != nil {
		t.Fatalf("status: %d %s", response.Code, response.Body.String())
	}
	if len(status.ProjectErrors) != 2 || len(status.States) != 3 {
		t.Fatalf("status lost an independent share: %+v %+v", status.ProjectErrors, status.States)
	}
	seen := map[string]string{}
	for _, state := range status.States {
		seen[state.Path] = state.State
	}
	if seen["/projects/a"] != projectShareReady || seen["/projects/b"] != projectShareDegraded || seen["/projects/c"] != projectShareDegraded {
		t.Fatalf("states: %+v", status.States)
	}
}

func TestRetireGuestMountIgnoresForeignFilesystems(t *testing.T) {
	owned := "cage-project-" + strings.Repeat("ab", 16)
	client := "cage-client-" + strings.Repeat("cd", 16)
	if retireGuestMount("ext4", "cage-project-0123456789abcdef", projectMountPrefix) || retireGuestMount("fuse.sshfs", "/home/cage", projectMountPrefix) || retireGuestMount("fuse.sshfs", "cage-project-", projectMountPrefix) {
		t.Fatal("foreign mount was eligible for removal")
	}
	if retireGuestMount("fuse.sshfs", "cage-project-0123456789abcdef", projectMountPrefix) || retireGuestMount("fuse.sshfs", owned+"00", projectMountPrefix) {
		t.Fatal("short or extended mount name was accepted")
	}
	if !retireGuestMount("fuse.sshfs", owned, projectMountPrefix) || !retireGuestMount("fuse.sshfs", client, clientMountPrefix) {
		t.Fatal("owned mount was not identified")
	}
}

func TestConcurrentProjectRecordsKeepDistinctRootsAndRemoval(t *testing.T) {
	s := testServer(t)
	a, b, removed := "project-a", "project-b", "project-removed"
	for range 40 {
		if err := writePrivate(filepath.Join(s.profile, "projects.json"), sharedProjects{Paths: []string{a, b, removed}}); err != nil {
			t.Fatal(err)
		}
		var group sync.WaitGroup
		group.Add(3)
		go func() {
			defer group.Done()
			if err := s.storeProjectRoot(a, projectRootID{Dev: 1, Ino: 11}); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer group.Done()
			if err := s.storeProjectRoot(b, projectRootID{Dev: 2, Ino: 22}); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer group.Done()
			if err := s.removeSavedProjectPath(removed); err != nil {
				t.Error(err)
			}
		}()
		group.Wait()
		saved, err := s.loadSharedProjects()
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(saved.Paths, removed) || !slices.Contains(saved.Paths, a) || !slices.Contains(saved.Paths, b) {
			t.Fatalf("paths after concurrent update: %+v", saved.Paths)
		}
		roots := map[string]projectRootID{}
		for _, root := range saved.Roots {
			roots[root.Path] = projectRootID{Dev: root.Dev, Ino: root.Ino}
		}
		if roots[a] != (projectRootID{Dev: 1, Ino: 11}) || roots[b] != (projectRootID{Dev: 2, Ino: 22}) || roots[removed] != (projectRootID{}) {
			t.Fatalf("roots after concurrent update: %+v", saved.Roots)
		}
	}
}
