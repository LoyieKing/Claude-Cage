package workbench

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/containerenv"
	"golang.org/x/net/http/httpguts"
)

// This namespace exists only in the Linux guest. Never resolve its paths on
// the server host or persist them as grants across connections.
const clientMountRoot = "/run/cage-client-projects"

var clientMountID = regexp.MustCompile(`^[0-9a-f]{32}$`)
var errClientMountGone = errors.New("本机项目共享已断开，请在本机重新选择文件夹")

type guestClientMount struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	ready  bool // protected by Server.mu
}

// Caller holds Server.mu. Cancellation closes admission before unmount cleanup.
func (m *guestClientMount) live() bool { return m.ready && m.ctx.Err() == nil }

func isClientMountPath(path string) bool {
	return path == clientMountRoot || strings.HasPrefix(path, clientMountRoot+"/")
}

func (s *Server) clientMountWorkspace(path string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mount := s.clientMounts[path]; mount != nil && mount.live() {
		return path, nil
	}
	return "", errClientMountGone
}

func (s *Server) appendClientMounts(paths []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var live []string
	for path, mount := range s.clientMounts {
		if mount.live() && !slices.Contains(paths, path) {
			live = append(live, path)
		}
	}
	sort.Strings(live)
	return append(paths, live...)
}

func (s *Server) closeClientMounts() {
	s.mu.Lock()
	var mounts []*guestClientMount
	for _, m := range s.clientMounts {
		m.cancel()
		mounts = append(mounts, m)
	}
	s.mu.Unlock()
	for _, m := range mounts {
		<-m.done
	}
}

// After a controller crash a disconnected FUSE mount may outlive its stream.
// Only retire exact Cage names in the guest's reserved, root-owned directory.
// rmdir preserves any unexpected nonempty directory.
func (s *Server) recoverClientMounts(ctx context.Context) error {
	s.mu.Lock()
	initialized := s.clientMounts != nil
	s.mu.Unlock()
	if initialized {
		return nil
	}
	recoverScript := `set -eu
root=` + clientMountRoot + `
[ ! -L "$root" ] || exit 1
[ -d "$root" ] || exit 0
for p in "$root"/*; do
  [[ ${p##*/} =~ ^[a-f0-9]{32}$ ]] || continue
  [ ! -L "$p" ] || continue
  kind=$(findmnt -rn --nocanonicalize -o FSTYPE --mountpoint "$p") || kind=
  src=$(findmnt -rn --nocanonicalize -o SOURCE --mountpoint "$p" 2>/dev/null) || src=
  case "$kind:$src" in
    fuse.sshfs:cage-client-` + exactMountHex() + `) sudo -n umount -- "$p" || sudo -n umount -l -- "$p" || true;;
    *) continue;;
  esac
  if findmnt -rn --nocanonicalize --mountpoint "$p" >/dev/null 2>&1; then continue; fi
  sudo -n rmdir -- "$p" || true
done`
	if _, err := s.guest(ctx, recoverScript); err != nil {
		return errors.New("无法清理上次连接遗留的本机项目挂载")
	}
	s.mu.Lock()
	if s.clientMounts == nil {
		s.clientMounts = make(map[string]*guestClientMount)
	}
	s.mu.Unlock()
	return nil
}

func (s *Server) guestClientShare(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "linux" || !containerenv.Configured(s.profile) {
		http.NotFound(w, r)
		return
	}
	id := r.URL.Query().Get("id")
	if !clientMountID.MatchString(id) {
		http.Error(w, "本机项目标识无效", http.StatusBadRequest)
		return
	}
	path := clientMountRoot + "/" + id
	if r.Method == http.MethodGet {
		if _, err := s.clientMountWorkspace(path); err != nil {
			http.Error(w, "本机项目尚未连接", http.StatusConflict)
			return
		}
		jsonReply(w, map[string]string{"workspace": path})
		return
	}
	if !httpguts.HeaderValuesContainsToken(r.Header.Values("Connection"), "upgrade") || !strings.EqualFold(r.Header.Get("Upgrade"), "cage-sftp-v1") || r.ContentLength != 0 {
		http.Error(w, "需要项目共享连接", http.StatusBadRequest)
		return
	}
	if !s.op.TryLock() {
		http.Error(w, "环境正在处理其他操作", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	mount := &guestClientMount{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	available := s.ready && !s.closing && len(s.clientMounts) < 16 && s.clientMounts[path] == nil && !s.busy()
	if available {
		if s.clientMounts == nil {
			s.clientMounts = make(map[string]*guestClientMount)
		}
		s.clientMounts[path] = mount
	}
	s.mu.Unlock()
	s.op.Unlock()
	if !available {
		http.Error(w, "环境未就绪、共享已存在或已达共享上限", http.StatusConflict)
		return
	}
	defer func() {
		s.mu.Lock()
		delete(s.clientMounts, path)
		// Keep a revoked active selection until another project is selected.
		// New tasks must fail, not silently start in the previous cloud folder.
		s.mu.Unlock()
		close(mount.done)
	}()

	// A disconnected mount exposes an inaccessible underlying directory until
	// teardown removes it. Never let an old cwd accept writes into the cloud.
	// The grant ends with this request. It is not restored after reconnect.
	fsname := clientMountPrefix + id
	setup, finishSetup := context.WithTimeout(ctx, 20*time.Second)
	client, err := s.dialGuestSSH(setup)
	if err != nil {
		finishSetup()
		http.Error(w, "无法连接云端容器", http.StatusConflict)
		return
	}
	err = prepareGuestMount(setup, client, path, fsname, clientMountPrefix)
	finishSetup()
	if err != nil {
		_ = client.Close()
		http.Error(w, "无法准备云端容器挂载位置", http.StatusConflict)
		return
	}
	defer client.Close()
	defer func() {
		s.retireClientMount(path, fsname)
	}()
	conn, buffer, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "无法建立项目共享数据流", http.StatusServiceUnavailable)
		return
	}
	defer conn.Close()
	command, err := sshfsCommand(":/", path, fsname)
	if err != nil {
		return
	}
	session, stdout, stdin, stopAttach, err := startGuestSSHFS(ctx, client, command)
	if err != nil {
		return
	}
	toGuest := make(chan struct{})
	toHost := make(chan struct{})
	go func() {
		defer close(toGuest)
		_, _ = io.Copy(stdin, buffer.Reader)
	}()
	go func() {
		defer close(toHost)
		_, _ = io.Copy(conn, stdout)
	}()
	sessionDone := make(chan struct{})
	waited := false
	defer func() {
		stopAttach()
		_ = client.Close()
		<-toGuest
		<-toHost
		if waited {
			<-sessionDone
		} else {
			_ = session.Wait()
		}
	}()
	if _, err = buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: cage-sftp-v1\r\n\r\n"); err != nil || buffer.Flush() != nil {
		return
	}
	go func() {
		_ = session.Wait()
		close(sessionDone)
	}()
	waited = true
	probed := make(chan struct{})
	go func() {
		defer close(probed)
		probe, end := context.WithTimeout(ctx, 15*time.Second)
		defer end()
		if err := waitGuestMount(probe, client, guestMountCheckScript(path, fsname, "", "")); err != nil {
			_ = client.Close()
			return
		}
		s.mu.Lock()
		mount.ready = ctx.Err() == nil && s.clientMounts[path] == mount
		s.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
	case <-sessionDone:
	}
	<-probed
}
