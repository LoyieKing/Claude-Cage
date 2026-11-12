package workbench

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/remotehost"
)

// The shell owns connection views. The local controller owns its workload,
// the original configuration and the stop/handoff decision.
type DesktopLocalController interface {
	Status(context.Context) (DesktopLocalStatus, error)
	Connect(context.Context) (*remotehost.Session, error)
	Release(context.Context, DesktopLocalStatus, bool) error
	ReleaseOwnedIdle(context.Context, DesktopLocalStatus) (bool, error)
}

type DesktopLocalStatus struct {
	Available      bool   `json:"available"`
	Known          bool   `json:"known"`
	ServiceRunning bool   `json:"service_running"`
	Running        bool   `json:"running"`
	Busy           bool   `json:"busy"`
	LiveTerminals  int    `json:"live_terminals"`
	ControllerID   string `json:"controller_id,omitempty"`
	Message        string `json:"message,omitempty"`
}

var (
	errDesktopLocalLocked = errors.New("请先切回本机解锁账户，再停止本机环境")
	errViewChanged        = errors.New("工作环境已切换，请刷新后重试")
	errCandidateChanged   = errors.New("候选连接已取消或发生变化，请重新选择")
)

type desktopCandidate struct {
	ID         string                     `json:"id"`
	Mode       string                     `json:"mode"`
	Connection remotehost.ConnectionState `json:"connection"`
}

// One target per connection generation. An SSH candidate has its own manager,
// so opening a second host never closes or reroutes the current workbench.
type desktopTarget struct {
	id, mode  string
	remote    *remotehost.Manager
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	session   *remotehost.Session
	localErr  error
	localDone chan struct{}
}

func (t *desktopTarget) state() remotehost.ConnectionState {
	if t.remote != nil {
		return t.remote.Snapshot()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := remotehost.ConnectionState{State: "connecting", ConnectionID: t.id, Phase: "local_connect", Message: "正在连接本机工作台"}
	switch {
	case t.localErr != nil:
		state.State, state.Phase, state.ErrorCode, state.Message = "failed", "", "local_unavailable", t.localErr.Error()
	case t.session != nil && t.session.Context.Err() == nil:
		manifest := t.session.Manifest
		state.State, state.Phase, state.Message = "connected", "", "已连接本机环境"
		state.InstanceID, state.Manifest = manifest.InstanceID, &manifest
	case t.session != nil || t.ctx.Err() != nil:
		state.State, state.Phase, state.ErrorCode, state.Message = "failed", "", "connection_lost", "本机工作台连接已断开，请重新连接"
	}
	return state
}

func (t *desktopTarget) acquire() (*remotehost.Session, error) {
	if t.remote != nil {
		// t.id is the manager's live ConnectionID. Callers that read it for an
		// active remote target hold desktopGateway.mu, which also covers rotation.
		return t.remote.Acquire(t.id)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.session == nil || t.ctx.Err() != nil || t.session.Context.Err() != nil {
		return nil, errors.New("本机工作台尚未连接")
	}
	return t.session, nil
}

func (t *desktopTarget) close() {
	if t == nil {
		return
	}
	t.cancel()
	if t.remote != nil {
		_ = t.remote.Close()
		return
	}
	if t.localDone != nil {
		<-t.localDone
	}
	t.mu.Lock()
	s := t.session
	t.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

func (t *desktopTarget) candidate() *desktopCandidate {
	if t == nil {
		return nil
	}
	return &desktopCandidate{ID: t.id, Mode: t.mode, Connection: t.state()}
}

type desktopGateway struct {
	// A slow stop can serialize selection changes, but status and requests for
	// the current workbench must remain available while it finishes.
	change          sync.Mutex
	mu              sync.RWMutex
	store           *remotehost.Store
	local           DesktopLocalController
	generation      string
	active, pending *desktopTarget
	closed          bool
	departing       bool
}

func newDesktopGateway(profile string, local DesktopLocalController) *remoteGateway {
	g := newRemoteGateway(profile)
	g.desktop = &desktopGateway{local: local, store: g.store, generation: randomToken()}
	return g
}

func (d *desktopGateway) context() ClientContext {
	d.mu.Lock()
	d.rotateRemoteLocked()
	generation, active, pending := d.generation, d.active, d.pending
	d.mu.Unlock()
	c := ClientContext{DesktopGateway: true, Mode: "none", ConnectionID: generation, ClientPlatform: runtime.GOOS, State: remotehost.ConnectionState{State: "disconnected"}, Candidate: pending.candidate()}
	if active == nil {
		return c
	}
	c.Mode, c.State = active.mode, active.state()
	c.HostID, c.InstanceID = c.State.HostID, c.State.InstanceID
	if active.mode == "local" {
		c.Name, c.ServerPlatform = "本机环境", runtime.GOOS
	} else {
		c.ServerPlatform = "linux"
		if p, err := d.store.Get(c.HostID); err == nil {
			c.Name = p.Name
			if p.HostKey != nil {
				c.HostFingerprint = p.HostKey.Fingerprint
			}
		}
	}
	return c
}

type switchRequest struct {
	ConnectionID      string `json:"connection_id"`
	CandidateID       string `json:"candidate_id,omitempty"`
	Mode              string `json:"mode,omitempty"`
	HostID            string `json:"host_id,omitempty"`
	Deploy            bool   `json:"deploy,omitempty"`
	Passphrase        string `json:"passphrase,omitempty"`
	LocalAction       string `json:"local_action,omitempty"`
	LocalControllerID string `json:"local_controller_id,omitempty"`
}

func (d *desktopGateway) checkGeneration(id string) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed || id == "" || id != d.generation {
		return errViewChanged
	}
	return nil
}

// The selected route stays available throughout the candidate handshake.
func (d *desktopGateway) prepare(in switchRequest) (*desktopCandidate, error) {
	d.change.Lock()
	defer d.change.Unlock()
	if err := d.checkGeneration(in.ConnectionID); err != nil {
		return nil, err
	}
	if d.pending != nil {
		return nil, errors.New("请先取消当前候选连接")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &desktopTarget{mode: in.Mode, ctx: ctx, cancel: cancel}
	switch in.Mode {
	case "remote":
		t.remote = remotehost.NewManager(d.store)
		t.remote.SetRecovery(false)
		secret := []byte(in.Passphrase)
		st, err := t.remote.Connect(in.HostID, remotehost.ConnectSecrets{Passphrase: secret, Deploy: in.Deploy})
		clear(secret)
		if err != nil {
			t.close()
			return nil, err
		}
		t.id = st.ConnectionID
	case "local":
		if d.active != nil && d.active.mode == "local" && d.active.state().State == "connected" {
			t.close()
			return nil, errors.New("已连接当前本机工作台")
		}
		t.id = randomToken()
		if d.local == nil {
			t.close()
			return nil, errors.New("本机运行组件不可用")
		}
		t.localDone = make(chan struct{})
		go func() {
			defer close(t.localDone)
			session, err := d.local.Connect(ctx)
			t.mu.Lock()
			t.session, t.localErr = session, err
			t.mu.Unlock()
		}()
	default:
		t.close()
		return nil, errors.New("请选择本机或远程主机")
	}
	d.mu.Lock()
	d.pending = t
	d.mu.Unlock()
	return t.candidate(), nil
}

func (d *desktopGateway) matchesCandidate(in switchRequest) (*desktopTarget, error) {
	if err := d.checkGeneration(in.ConnectionID); err != nil {
		return nil, err
	}
	if d.pending == nil || in.CandidateID == "" || in.CandidateID != d.pending.id {
		return nil, errCandidateChanged
	}
	return d.pending, nil
}

func (d *desktopGateway) cancelCandidate(in switchRequest) error {
	d.change.Lock()
	defer d.change.Unlock()
	t, err := d.matchesCandidate(in)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.pending = nil
	d.mu.Unlock()
	d.discard(t)
	return nil
}

// A cancelled local candidate may have started an idle controller. Its owner
// can release that exact fresh process after rechecking its private launch ID.
func (d *desktopGateway) discard(t *desktopTarget) {
	t.close()
	if t == nil || t.mode != "local" || t.session == nil {
		return
	}
	if owner, ok := d.local.(interface {
		ReleaseUnused(context.Context, *remotehost.Session) error
	}); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := owner.ReleaseUnused(ctx, t.session); err != nil {
			diagnostics.Log("warn", "controller", "取消本机候选后未能确认空闲服务已释放")
		}
	}
}

func (d *desktopGateway) stopLocal(ctx context.Context, expected string) error {
	if d.local == nil || expected == "" {
		return errors.New("无法确认本机后台身份，请刷新本机状态")
	}
	st, err := d.local.Status(ctx)
	if err != nil {
		return err
	}
	if !st.Known || st.ControllerID != expected {
		return errors.New("本机后台已变化，请刷新状态后再停止")
	}
	return d.local.Release(ctx, st, true)
}

func (d *desktopGateway) stopLocalCard(ctx context.Context, in switchRequest) error {
	d.change.Lock()
	defer d.change.Unlock()
	if err := d.checkGeneration(in.ConnectionID); err != nil {
		return err
	}
	selected := d.active != nil && d.active.mode == "local"
	if selected {
		resume := d.freezeView()
		defer resume()
	}
	if err := d.stopLocal(ctx, in.LocalControllerID); err != nil {
		return err
	}
	if selected {
		d.clearSelection()
	}
	return nil
}

func suspendLocalView(ctx context.Context, t *desktopTarget) error {
	session, err := t.acquire()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, session.Origin+"/api/account/suspend", strings.NewReader("{}"))
	if err != nil {
		return err
	}
	req.Header.Set("Origin", session.Origin)
	req.Header.Set("Content-Type", "application/json")
	response, err := session.Client.Do(req)
	if err != nil {
		return errors.New("未能锁定本机会话，请重新连接本机工作台")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusLocked {
		return errors.New("本机工作台未确认账户锁定")
	}
	return nil
}

func (d *desktopGateway) leaveLocal(ctx context.Context, in switchRequest, old *desktopTarget) error {
	switch in.LocalAction {
	case "stop":
		return d.stopLocal(ctx, in.LocalControllerID)
	case "keep":
		if old != nil && old.mode == "local" {
			return suspendLocalView(ctx, old)
		}
		return nil
	case "", "auto":
		if d.local == nil {
			return nil
		}
		st, err := d.local.Status(ctx)
		if err != nil || !st.Known || st.Running || st.Busy || st.LiveTerminals > 0 {
			return errors.New("本机环境可能仍有任务，请选择保留运行或停止本机")
		}
		if !st.ServiceRunning {
			return nil
		}
		released, err := d.local.ReleaseOwnedIdle(ctx, st)
		if err != nil {
			return err
		}
		if !released && old != nil && old.mode == "local" && old.state().State == "connected" {
			return suspendLocalView(ctx, old)
		}
		return nil
	default:
		return errors.New("本机处理方式无效")
	}
}

func (d *desktopGateway) commit(ctx context.Context, in switchRequest) error {
	d.change.Lock()
	defer d.change.Unlock()
	t, err := d.matchesCandidate(in)
	if err != nil {
		return err
	}
	if _, err = t.acquire(); err != nil {
		return errors.New("候选工作台尚未连接，当前工作台已保留")
	}
	old := d.active
	if old != nil && old.mode == "local" {
		resume := d.freezeView()
		defer resume()
	}
	if t.mode == "remote" {
		if err = d.leaveLocal(ctx, in, old); err != nil {
			return err
		}
	}
	if t.remote != nil {
		if err = t.remote.Publish(); err != nil {
			return errors.New("候选工作台连接已中断，请重新连接")
		}
	} else if _, err = t.acquire(); err != nil {
		return errors.New("候选工作台连接已中断，请重新连接")
	}
	d.mu.Lock()
	if t.remote != nil {
		if state := t.remote.Snapshot(); state.State == "connected" && state.ConnectionID != "" {
			t.id = state.ConnectionID
		}
	}
	d.generation, d.active, d.pending = t.id, t, nil
	d.mu.Unlock()
	old.close()
	return nil
}

// While a confirmed switch releases or suspends the old controller, another
// window must not start new work through that view. A failed commit reopens
// admission to the same generation.
func (d *desktopGateway) freezeView() func() {
	d.mu.Lock()
	d.departing = true
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		d.departing = false
		d.mu.Unlock()
	}
}

func (d *desktopGateway) disconnect(ctx context.Context, in switchRequest) error {
	d.change.Lock()
	defer d.change.Unlock()
	if err := d.checkGeneration(in.ConnectionID); err != nil {
		return err
	}
	resume := d.freezeView()
	defer resume()
	if t := d.active; t != nil && t.mode == "local" && t.state().State == "connected" {
		if err := suspendLocalView(ctx, t); err != nil {
			return err
		}
	}
	d.clearSelection()
	return nil
}

// Called with change locked. Invalidate before closing transports so stale
// callbacks cannot submit commands to a newly chosen target.
func (d *desktopGateway) clearSelection() {
	d.mu.Lock()
	old, pending := d.active, d.pending
	d.active, d.pending, d.generation = nil, nil, randomToken()
	d.mu.Unlock()
	d.discard(old)
	d.discard(pending)
}

func (d *desktopGateway) acquire(id string) (*remotehost.Session, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rotateRemoteLocked()
	if d.closed || d.active == nil {
		return nil, false, errViewChanged
	}
	if d.departing || id == "" || id != d.generation {
		return nil, d.active.mode == "remote", errViewChanged
	}
	session, err := d.active.acquire()
	return session, d.active.mode == "remote", err
}

// rotateRemoteLocked pins the desktop view to the manager's published
// ConnectionID. A retired id stops matching immediately, before any UI poll.
func (d *desktopGateway) rotateRemoteLocked() {
	if d.closed || d.departing || d.active == nil || d.active.remote == nil {
		return
	}
	state := d.active.remote.Snapshot()
	if state.State != "connected" || state.ConnectionID == "" || state.ConnectionID == d.active.id {
		return
	}
	d.active.id = state.ConnectionID
	d.generation = state.ConnectionID
}

func (d *desktopGateway) close() {
	d.change.Lock()
	defer d.change.Unlock()
	if d.closed {
		return
	}
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	if t := d.active; t != nil && t.mode == "local" && t.state().State == "connected" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = suspendLocalView(ctx, t)
		cancel()
	}
	d.clearSelection()
}

func (d *desktopGateway) handle(path string, w http.ResponseWriter, r *http.Request) bool {
	if path == "trusted-login" {
		d.trustedLogin(w, r)
		return true
	}
	if path == "local-status" && r.Method == http.MethodGet {
		if d.local == nil {
			jsonReply(w, DesktopLocalStatus{Known: true})
			return true
		}
		st, err := d.local.Status(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return true
		}
		jsonReply(w, st)
		return true
	}
	switch path {
	case "candidate", "commit", "cancel", "disconnect", "local-stop":
	case "local", "connect":
		http.Error(w, "请通过候选连接选择工作环境", http.StatusConflict)
		return true
	default:
		return false
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	var in switchRequest
	if !readJSON(w, r, &in) {
		return true
	}
	var err error
	switch path {
	case "candidate":
		var c *desktopCandidate
		c, err = d.prepare(in)
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			jsonReply(w, c)
			return true
		}
	case "commit":
		err = d.commit(r.Context(), in)
	case "cancel":
		err = d.cancelCandidate(in)
	case "disconnect":
		err = d.disconnect(r.Context(), in)
	case "local-stop":
		err = d.stopLocalCard(r.Context(), in)
	}
	if err != nil {
		code := http.StatusConflict
		if errors.Is(err, errDesktopLocalLocked) {
			code = http.StatusLocked
		}
		http.Error(w, err.Error(), code)
		return true
	}
	jsonReply(w, d.context())
	return true
}
