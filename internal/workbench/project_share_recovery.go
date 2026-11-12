package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/lima-vm/sshocker/pkg/reversesshfs"
	"github.com/sylvinus/agent-vm/internal/containerenv"
	"golang.org/x/crypto/ssh"
)

const (
	projectShareMounting   = "mounting"
	projectShareReady      = "ready"
	projectShareDegraded   = "degraded"
	projectShareRetiring   = "retiring"
	projectShareRecovering = "recovering"
	projectShareRevoked    = "revoked"
	projectShareBlocked    = "blocked"
	projectShareStopped    = "stopped"

	projectMountPrefix = "cage-project-"
	clientMountPrefix  = "cage-client-"
)

var errProjectRootReplaced = errors.New("项目目录已被替换，拒绝重新挂载")
var errProjectShareRevoked = errors.New("项目授权已结束")

// projectShareBackoff is the capped delay before an independent share retry.
// Shutdown and revocation interrupt the wait; a late success cannot publish.
var projectShareBackoff = func(attempt int) time.Duration {
	delay := 250 * time.Millisecond
	for range attempt {
		if delay >= 5*time.Second {
			return 5 * time.Second
		}
		delay *= 2
	}
	if delay > 5*time.Second {
		return 5 * time.Second
	}
	return delay
}

type projectRootID struct {
	Dev uint64
	Ino uint64
}

type projectRootRecord struct {
	Path string `json:"path"`
	Dev  uint64 `json:"dev"`
	Ino  uint64 `json:"ino"`
}

type projectShareState struct {
	Path       string `json:"path"`
	State      string `json:"state"`
	Generation uint64 `json:"generation,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (s *Server) supportsLinuxProjectShare() bool {
	return s.windowsGuest == nil && s.macosGuest == nil
}

// defaultEnvironmentShareReady admits new work in the default directory only
// while the container network owner reports the share ready. It does not own
// that share and does not clear the workbench ready flag when the share is down.
func (s *Server) defaultEnvironmentShareReady() error {
	if !containerenv.Configured(s.profile) {
		return nil
	}
	environment, err := containerenv.Load(s.profile)
	if err != nil {
		return errors.New("默认工作区共享尚未就绪，其他项目可继续使用")
	}
	ctx, cancel := context.WithTimeout(s.lifetimeContext(), 3*time.Second)
	defer cancel()
	state, err := environment.State(ctx)
	if err != nil || !state.Shared {
		return errors.New("默认工作区共享尚未就绪，其他项目可继续使用")
	}
	return nil
}

// recoverProjectShares restarts saved Linux project shares whose watcher is
// not already running. It does not start the environment directory, client
// mounts, revoked grants, or a share after shutdown has begun.
func (s *Server) recoverProjectShares(ctx context.Context) {
	if ctx.Err() != nil || !s.supportsLinuxProjectShare() {
		return
	}
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		return
	}
	saved, err := s.loadSharedProjects()
	if err != nil {
		return
	}
	for _, path := range saved.Paths {
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		closing = s.closing
		s.mu.Unlock()
		if closing || path == "" || path == s.environmentDir() || isClientMountPath(path) {
			if closing {
				return
			}
			continue
		}
		if _, err := validateWorkspace(path, s.profile); err != nil {
			continue
		}
		s.mu.Lock()
		share := s.projectShares[path]
		skip := share.liveLocked() || (share != nil && (share.linuxManaged || share.state == projectShareBlocked || share.state == projectShareRevoked))
		s.mu.Unlock()
		if skip {
			continue
		}
		_ = s.ensureLinuxProjectShare(path)
	}
}

func (s *Server) ensureLinuxProjectShare(path string) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return errJobsClosed
	}
	current := s.projectShares[path]
	if current.liveLocked() {
		s.mu.Unlock()
		return nil
	}
	if current != nil && current.linuxManaged {
		err := shareFailure(current)
		s.mu.Unlock()
		return err
	}
	var stale *projectShare
	if current != nil {
		stale = s.sealProjectShareLocked(path, projectShareRevoked)
	}
	s.mu.Unlock()
	if stale != nil {
		if stale.cancel != nil {
			stale.cancel()
		}
		stale.close()
	}
	return s.startLinuxProjectShare(path)
}

func (s *Server) startLinuxProjectShare(path string) error {
	ctx, cancel := context.WithCancel(s.lifetimeContext())
	share := &projectShare{
		path: path, ctx: ctx, cancel: cancel, linuxManaged: true, admit: true,
		state: projectShareMounting, done: make(chan struct{}), ready: make(chan struct{}), firstDone: make(chan struct{}),
	}
	granted := s.projectGrantCurrent(path)
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		cancel()
		return errJobsClosed
	}
	if current := s.projectShares[path]; current != nil {
		err := shareFailure(current)
		if current.liveLocked() {
			err = nil
		}
		s.mu.Unlock()
		cancel()
		return err
	}
	if s.projectShares == nil {
		s.projectShares = map[string]*projectShare{}
	}
	if s.projectShareEpoch == nil {
		s.projectShareEpoch = map[string]uint64{}
	}
	share.epoch = s.projectShareEpoch[path]
	share.allowUnsaved = !granted
	s.projectShares[path] = share
	s.mu.Unlock()
	go s.runLinuxProjectShare(share)
	return s.waitLinuxProjectShare(share)
}

func (s *Server) waitLinuxProjectShare(share *projectShare) error {
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case <-share.ready:
		return nil
	case <-share.firstDone:
		return s.shareError(share)
	case <-share.ctx.Done():
		return errProjectShareRevoked
	case <-timer.C:
		return errors.New("项目文件夹连接超时，原有环境与会话保持运行")
	}
}

func (s *Server) runLinuxProjectShare(share *projectShare) {
	defer close(share.done)
	defer share.signalFirst()
	skippedGrant := share.allowUnsaved
	attempt := 0
	for {
		if share.ctx.Err() != nil || !s.shareAdmitted(share) {
			return
		}
		if !skippedGrant && !s.projectGrantCurrent(share.path) {
			s.sealProjectShare(share.path)
			return
		}
		skippedGrant = false
		if attempt == 0 {
			s.noteShareState(share, projectShareMounting, "")
		} else {
			s.noteShareState(share, projectShareRecovering, "")
		}
		err := s.mountLinuxProjectOnce(share)
		if share.ctx.Err() != nil || !s.shareAdmitted(share) {
			share.signalFirst()
			return
		}
		if errors.Is(err, errProjectRootReplaced) {
			s.noteShareState(share, projectShareBlocked, err.Error())
			s.noteProjectShareError(share, err)
			share.signalFirst()
			return
		}
		if errors.Is(err, errProjectShareRevoked) {
			share.signalFirst()
			s.sealProjectShare(share.path)
			return
		}
		if err == nil {
			err = errors.New("项目共享已断开")
		}
		s.noteShareState(share, projectShareDegraded, err.Error())
		s.noteProjectShareError(share, err)
		share.signalFirst()
		timer := time.NewTimer(projectShareBackoff(attempt))
		attempt++
		select {
		case <-share.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Server) mountLinuxProjectOnce(share *projectShare) error {
	if err := s.authorizeLinuxProject(share); err != nil {
		return err
	}
	setup, setupCancel := context.WithTimeout(share.ctx, 20*time.Second)
	defer setupCancel()
	client, err := s.dialGuestSSH(setup)
	if err != nil {
		return err
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { _ = client.Close() }) }
	share.setRelease(release)
	defer release()
	mountID := randomToken()[:32]
	fsname := projectMountPrefix + mountID
	guestDir := guestPath(share.path)
	if err := prepareGuestMount(setup, client, guestDir, fsname, projectMountPrefix); err != nil {
		return errors.New("无法在 Cage 中准备项目目录")
	}
	defer s.retireProjectMount(guestDir, fsname)
	if err := s.authorizeLinuxProject(share); err != nil {
		return err
	}
	probe, err := os.CreateTemp(share.path, ".cage-mount-probe-*")
	if err != nil {
		return errors.New("无法验证项目目录的读写权限")
	}
	defer os.Remove(probe.Name())
	challenge := randomToken()
	if _, err = probe.WriteString(challenge); err != nil {
		_ = probe.Close()
		return err
	}
	if err = probe.Close(); err != nil {
		return err
	}
	if err := s.authorizeLinuxProject(share); err != nil {
		return err
	}
	command, err := sshfsCommand(":"+filepath.ToSlash(share.path), guestDir, fsname)
	if err != nil {
		return err
	}
	session, stdout, stdin, stopAttach, err := startGuestSSHFS(share.ctx, client, command)
	if err != nil {
		return errors.New("项目文件夹未能连接，原有环境与会话保持运行")
	}
	var retireOnce sync.Once
	retireSession := func() {
		retireOnce.Do(func() {
			if stopAttach != nil {
				stopAttach()
			}
			release()
			_ = session.Close()
		})
	}
	transport := projectTransport{
		ReadCloser:  closingReader{Reader: stdout, close: retireSession},
		WriteCloser: stdin,
		detachWrite: true,
	}
	share.releaseMu.Lock()
	share.release = retireSession
	share.transport = transport
	share.releaseMu.Unlock()
	rootInfo, rootID, err := openProjectRoot(share.path)
	if err != nil {
		retireSession()
		return errors.New("无法读取项目目录身份")
	}
	saved, pinned, err := s.savedProjectRoot(share.path)
	if err != nil || !pinned || saved != rootID {
		retireSession()
		if err != nil {
			return err
		}
		return errProjectRootReplaced
	}
	served := make(chan error, 1)
	go func() {
		err := reversesshfs.ServeConfinedPinned(transport, share.path, false, nil, rootInfo)
		if errors.Is(err, reversesshfs.ErrPinnedRootMismatch) {
			err = errProjectRootReplaced
		}
		served <- err
	}()
	verify, verifyCancel := context.WithTimeout(share.ctx, 15*time.Second)
	err = waitGuestMount(verify, client, guestMountCheckScript(guestDir, fsname, guestPath(probe.Name()), challenge))
	verifyCancel()
	if err != nil {
		retireSession()
		<-served
		_ = session.Wait()
		if share.ctx.Err() != nil {
			return errProjectShareRevoked
		}
		return errors.New("项目文件夹未能连接，原有环境与会话保持运行")
	}
	if err := s.authorizeLinuxProject(share); err != nil {
		retireSession()
		<-served
		_ = session.Wait()
		return err
	}
	if !s.markProjectShareReady(share) {
		retireSession()
		<-served
		_ = session.Wait()
		return errProjectShareRevoked
	}
	select {
	case <-share.ctx.Done():
		retireSession()
		<-served
		_ = session.Wait()
		return errProjectShareRevoked
	case err := <-served:
		retireSession()
		_ = session.Wait()
		if authErr := s.authorizeLinuxProject(share); errors.Is(authErr, errProjectRootReplaced) || errors.Is(authErr, errProjectShareRevoked) {
			return authErr
		}
		if err == nil {
			err = errors.New("项目共享已断开")
		}
		return err
	}
}

func (s *Server) authorizeLinuxProject(share *projectShare) error {
	if !s.shareAdmitted(share) {
		return errProjectShareRevoked
	}
	id, err := readProjectRootID(share.path)
	if err != nil {
		return errors.New("无法读取项目目录身份")
	}
	saved, pinned, err := s.savedProjectRoot(share.path)
	if err != nil {
		return err
	}
	if !pinned {
		return s.storeProjectRoot(share.path, id)
	}
	if saved != id {
		return errProjectRootReplaced
	}
	return nil
}

func (s *Server) confirmProjectRoot(path string) error {
	id, err := readProjectRootID(path)
	if err != nil {
		return errors.New("无法确认项目目录身份")
	}
	previous, pinned, err := s.savedProjectRoot(path)
	if err != nil {
		return err
	}
	if err := s.storeProjectRoot(path, id); err != nil {
		return err
	}
	if !pinned || previous == id {
		return nil
	}
	sealed := s.sealProjectShare(path)
	if sealed != nil {
		sealed.close()
	}
	return nil
}

func (p *projectShare) liveLocked() bool {
	if p == nil || p.done == nil {
		return false
	}
	if p.state != "" && p.state != projectShareReady {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *projectShare) signalFirst() {
	p.firstOnce.Do(func() {
		if p.firstDone != nil {
			close(p.firstDone)
		}
	})
}

func (p *projectShare) setRelease(fn func()) {
	p.releaseMu.Lock()
	p.release = fn
	p.releaseMu.Unlock()
}

func shareFailure(share *projectShare) error {
	if share == nil {
		return errors.New("项目文件夹尚未连接，原有环境与会话保持运行")
	}
	switch share.state {
	case projectShareBlocked:
		return errProjectRootReplaced
	case projectShareRevoked, projectShareStopped:
		return errProjectShareRevoked
	}
	if share.errText != "" {
		return errors.New(share.errText)
	}
	return errors.New("项目文件夹尚未连接，原有环境与会话保持运行")
}

func (s *Server) shareError(share *projectShare) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return shareFailure(share)
}

func (s *Server) shareAdmitted(share *projectShare) bool {
	if share == nil || share.ctx == nil || share.ctx.Err() != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closing && share.admit && s.projectShares[share.path] == share && s.projectShareEpoch[share.path] == share.epoch
}

func (s *Server) sealProjectShare(path string) *projectShare {
	s.mu.Lock()
	share := s.sealProjectShareLocked(path, projectShareRevoked)
	s.mu.Unlock()
	if share != nil && share.cancel != nil {
		share.cancel()
	}
	return share
}

// Caller holds Server.mu. The share is removed and its generation bumped so a
// late retry cannot publish it again.
func (s *Server) sealProjectShareLocked(path, state string) *projectShare {
	share := s.projectShares[path]
	delete(s.projectShares, path)
	if s.projectShareEpoch == nil {
		s.projectShareEpoch = map[string]uint64{}
	}
	s.projectShareEpoch[path]++
	if share != nil {
		share.admit = false
		if state != "" {
			share.state = state
		}
	}
	return share
}

func (s *Server) noteShareState(share *projectShare, state, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || !share.admit || s.projectShares[share.path] != share || s.projectShareEpoch[share.path] != share.epoch {
		return
	}
	share.state = state
	if errText != "" || state == projectShareMounting || state == projectShareReady {
		share.errText = errText
	}
}

func (s *Server) noteProjectShareError(share *projectShare, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || !share.admit || s.projectShares[share.path] != share || s.projectShareEpoch[share.path] != share.epoch {
		return
	}
	if err != nil {
		share.errText = err.Error()
	} else {
		share.errText = ""
	}
	s.setProjectErrorLocked(share.path, err)
}

func (s *Server) markProjectShareReady(share *projectShare) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || share.ctx.Err() != nil || !share.admit || s.projectShares[share.path] != share || s.projectShareEpoch[share.path] != share.epoch {
		return false
	}
	share.state = projectShareReady
	share.errText = ""
	s.setProjectErrorLocked(share.path, nil)
	share.readyOnce.Do(func() {
		if share.ready != nil {
			close(share.ready)
		}
	})
	return true
}

func (s *Server) projectShareStatesLocked() []projectShareState {
	states := make([]projectShareState, 0)
	seen := map[string]bool{}
	for path, share := range s.projectShares {
		state := share.state
		if state == "" {
			if share.liveLocked() {
				state = projectShareReady
			} else {
				state = projectShareDegraded
			}
		}
		states = append(states, projectShareState{Path: path, State: state, Generation: share.epoch, Error: share.errText})
		seen[path] = true
	}
	for _, failure := range s.projectErrors {
		if seen[failure.Path] {
			continue
		}
		states = append(states, projectShareState{Path: failure.Path, State: projectShareDegraded, Error: failure.Message})
	}
	slices.SortFunc(states, func(a, b projectShareState) int { return strings.Compare(a.Path, b.Path) })
	return states
}

func (s *Server) loadSharedProjects() (sharedProjects, error) {
	var saved sharedProjects
	data, err := os.ReadFile(filepath.Join(s.profile, "projects.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return saved, nil
		}
		return saved, err
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		return saved, err
	}
	return saved, nil
}

func (s *Server) projectGrantCurrent(path string) bool {
	saved, err := s.loadSharedProjects()
	if err != nil {
		return false
	}
	for _, savedPath := range saved.Paths {
		if savedPath == path {
			return true
		}
	}
	return false
}

func (s *Server) savedProjectRoot(path string) (projectRootID, bool, error) {
	saved, err := s.loadSharedProjects()
	if err != nil {
		return projectRootID{}, false, err
	}
	for _, root := range saved.Roots {
		if root.Path == path {
			return projectRootID{Dev: root.Dev, Ino: root.Ino}, true, nil
		}
	}
	return projectRootID{}, false, nil
}

func (s *Server) storeProjectRoot(path string, id projectRootID) error {
	epoch := s.projectEpoch(path)
	return s.updateProjectRecords(path, epoch, func(saved *sharedProjects) error {
		for i := range saved.Roots {
			if saved.Roots[i].Path == path {
				saved.Roots[i].Dev = id.Dev
				saved.Roots[i].Ino = id.Ino
				return nil
			}
		}
		saved.Roots = append(saved.Roots, projectRootRecord{Path: path, Dev: id.Dev, Ino: id.Ino})
		return nil
	})
}

func (s *Server) dropProjectRoot(path string) error {
	return s.updateProjectRecords("", 0, func(saved *sharedProjects) error {
		kept := saved.Roots[:0]
		for _, root := range saved.Roots {
			if root.Path != path {
				kept = append(kept, root)
			}
		}
		saved.Roots = kept
		return nil
	})
}

func (s *Server) removeSavedProjectPath(path string) error {
	return s.updateProjectRecords("", 0, func(saved *sharedProjects) error {
		kept := saved.Paths[:0]
		for _, savedPath := range saved.Paths {
			if savedPath != path {
				kept = append(kept, savedPath)
			}
		}
		saved.Paths = kept
		return nil
	})
}

func (s *Server) projectEpoch(path string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.projectShareEpoch[path]
}

// updateProjectRecords reads, mutates, rechecks epoch, and writes projects.json
// under one lock. A caller-supplied snapshot must not be written: another
// path's removal would be resurrected.
func (s *Server) updateProjectRecords(path string, epoch uint64, mutate func(*sharedProjects) error) error {
	s.projectRecordsMu.Lock()
	defer s.projectRecordsMu.Unlock()
	if path != "" && s.projectEpoch(path) != epoch {
		return errProjectShareRevoked
	}
	saved, err := s.loadProjectRecordsLocked()
	if err != nil {
		return err
	}
	if err = mutate(&saved); err != nil {
		return err
	}
	if path != "" && s.projectEpoch(path) != epoch {
		return errProjectShareRevoked
	}
	return s.writeProjectRecordsLocked(saved)
}

func (s *Server) loadProjectRecordsLocked() (sharedProjects, error) {
	return s.loadSharedProjects()
}

func (s *Server) writeProjectRecordsLocked(saved sharedProjects) error {
	return writePrivate(filepath.Join(s.profile, "projects.json"), saved)
}

// retireGuestMount reports whether this mountpoint source is a Cage share of
// the named family. Other filesystems and foreign sshfs mounts are left alone.
func retireGuestMount(kind, source, prefix string) bool {
	if kind != "fuse.sshfs" || !strings.HasPrefix(source, prefix) {
		return false
	}
	id := strings.TrimPrefix(source, prefix)
	if len(id) != 32 {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func prepareGuestMount(ctx context.Context, client *ssh.Client, mountpoint, fsname, prefix string) error {
	if !retireGuestMount("fuse.sshfs", fsname, prefix) {
		return errors.New("挂载标识无效")
	}
	_, err := guestSSHOutput(ctx, client, prepareGuestMountScript(mountpoint, fsname, prefix))
	return err
}

func cleanupGuestMount(ctx context.Context, client *ssh.Client, mountpoint, fsname string) error {
	_, err := guestSSHOutput(ctx, client, cleanupGuestMountScript(mountpoint, fsname, true))
	return err
}

// retireProjectMount unmounts only this attempt's fsname. The mode 000
// directory stays so a removed mountpoint cannot fall through to a writable parent.
func (s *Server) retireProjectMount(mountpoint, fsname string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := s.dialGuestSSH(ctx)
	if err != nil {
		return
	}
	defer client.Close()
	_, _ = guestSSHOutput(ctx, client, cleanupGuestMountScript(mountpoint, fsname, false))
}

func (s *Server) retireClientMount(mountpoint, fsname string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := s.dialGuestSSH(ctx)
	if err != nil {
		return
	}
	defer client.Close()
	_, _ = guestSSHOutput(ctx, client, cleanupGuestMountScript(mountpoint, fsname, true))
}

func waitGuestMount(ctx context.Context, client *ssh.Client, script string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		out, err := guestSSHOutput(ctx, client, script)
		if err == nil && strings.TrimSpace(string(out)) == "ok" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func prepareGuestMountScript(mountpoint, fsname, prefix string) string {
	return `mp=` + shellescape.Quote(mountpoint) + `
name=` + shellescape.Quote(fsname) + `
pre=` + shellescape.Quote(prefix) + `
if [ -L "$mp" ]; then exit 1; fi
if findmnt -rn --nocanonicalize --mountpoint "$mp" >/dev/null 2>&1; then
  kind=$(findmnt -rn --nocanonicalize -o FSTYPE --mountpoint "$mp" 2>/dev/null) || exit 1
  src=$(findmnt -rn --nocanonicalize -o SOURCE --mountpoint "$mp" 2>/dev/null) || exit 1
  [ "$kind" = fuse.sshfs ] || exit 1
  case "$src" in
    "$pre"` + exactMountHex() + `) ;;
    *) exit 1 ;;
  esac
  sudo -n umount -- "$mp" || sudo -n umount -l -- "$mp" || exit 1
  if findmnt -rn --nocanonicalize --mountpoint "$mp" >/dev/null 2>&1; then exit 1; fi
fi
parent=$(dirname -- "$mp")
[ -n "$parent" ] && [ "$parent" != "$mp" ] || exit 1
if [ -L "$parent" ]; then exit 1; fi
sudo -n mkdir -p -m 0755 -- "$parent" || exit 1
if [ -e "$mp" ]; then
  if [ -L "$mp" ] || [ ! -d "$mp" ]; then exit 1; fi
else
  sudo -n mkdir -m 000 -- "$mp" || exit 1
fi
if [ -L "$mp" ] || [ ! -d "$mp" ]; then exit 1; fi
if findmnt -rn --nocanonicalize --mountpoint "$mp" >/dev/null 2>&1; then exit 1; fi
sudo -n chown root:root -- "$mp" || exit 1
sudo -n chmod 000 -- "$mp" || exit 1
`
}

func exactMountHex() string { return strings.Repeat("[0-9a-f]", 32) }

func cleanupGuestMountScript(mountpoint, fsname string, removeDir bool) string {
	script := `mp=` + shellescape.Quote(mountpoint) + `
name=` + shellescape.Quote(fsname) + `
if findmnt -rn --nocanonicalize --mountpoint "$mp" >/dev/null 2>&1; then
  kind=$(findmnt -rn --nocanonicalize -o FSTYPE --mountpoint "$mp" 2>/dev/null || true)
  src=$(findmnt -rn --nocanonicalize -o SOURCE --mountpoint "$mp" 2>/dev/null || true)
  if [ "$kind" = fuse.sshfs ] && [ "$src" = "$name" ]; then
    sudo -n umount -- "$mp" || sudo -n umount -l -- "$mp" || true
  fi
fi
`
	if removeDir {
		script += `if findmnt -rn --nocanonicalize --mountpoint "$mp" >/dev/null 2>&1; then
  exit 0
fi
sudo -n rmdir -- "$mp" || true
`
	}
	return script
}

func guestMountCheckScript(mountpoint, fsname, probe, challenge string) string {
	script := `mp=` + shellescape.Quote(mountpoint) + `
name=` + shellescape.Quote(fsname) + `
kind=$(findmnt -rn --nocanonicalize -o FSTYPE --mountpoint "$mp" 2>/dev/null || true)
src=$(findmnt -rn --nocanonicalize -o SOURCE --mountpoint "$mp" 2>/dev/null || true)
[ "$kind" = fuse.sshfs ] && [ "$src" = "$name" ] || exit 1
`
	if probe != "" {
		script += `got=$(cat -- ` + shellescape.Quote(probe) + `) || exit 1
[ "$got" = ` + shellescape.Quote(challenge) + ` ] || exit 1
`
	} else {
		script += `stat -- "$mp"/ >/dev/null 2>&1 || exit 1
`
	}
	script += "printf ok\n"
	return script
}

func sshfsCommand(remote, mountpoint, fsname string) (string, error) {
	if strings.ContainsAny(fsname, " \t\n'\"\\,$`()") {
		return "", errors.New("挂载标识无效")
	}
	return "exec sudo -n sshfs -f " + shellescape.Quote(remote) + " " + shellescape.Quote(mountpoint) +
		` -o slave,allow_other,cache=no,nodev,nosuid,fsname=` + fsname + `,uid="$(id -u)",gid="$(id -g)"`, nil
}

func guestBash(script string) string {
	return "exec /bin/bash --noprofile --norc -c " + shellescape.Quote(script)
}

func guestSSHOutput(ctx context.Context, client *ssh.Client, script string) ([]byte, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = client.Close()
		_ = session.Close()
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	if err = session.Start(guestBash(script)); err != nil {
		stop()
		_ = client.Close()
		_ = session.Close()
		return nil, err
	}
	buf := make([]byte, (64<<10)+1)
	n, readErr := io.ReadFull(stdout, buf)
	stop()
	if n > 64<<10 {
		_ = client.Close()
		_ = session.Wait()
		return nil, errors.New("guest control response exceeded its limit")
	}
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		_ = client.Close()
		_ = session.Wait()
		return nil, readErr
	}
	if err = session.Wait(); err != nil {
		return nil, err
	}
	return buf[:n], nil
}

type closingReader struct {
	io.Reader
	close func()
}

func (c closingReader) Close() error {
	if c.close != nil {
		c.close()
	}
	return nil
}

func startGuestSSHFS(ctx context.Context, client *ssh.Client, script string) (*ssh.Session, io.Reader, io.WriteCloser, func() bool, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		return nil, nil, nil, nil, err
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		return nil, nil, nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	if err = session.Start(guestBash(script)); err != nil {
		stop()
		_ = client.Close()
		_ = session.Close()
		return nil, nil, nil, nil, err
	}
	return session, stdout, stdin, stop, nil
}
