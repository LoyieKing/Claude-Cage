package remotehost

import (
	"context"
	"errors"
	"maps"
	"sync"
	"time"
)

// Test seams. Production defaults detect a silent peer without turning a
// brief network blip into a deploy or a credential prompt.
var (
	heartbeatInterval = 15 * time.Second
	heartbeatTimeout  = 8 * time.Second
	reconnectLimit    = 4
	reconnectBackoff  = func(attempt int) time.Duration {
		if attempt < 1 {
			attempt = 1
		}
		shift := attempt - 1
		if shift > 4 {
			shift = 4
		}
		d := 250 * time.Millisecond << shift
		if d > 5*time.Second {
			return 5 * time.Second
		}
		return d
	}
	// onAttachDial runs at the start of an attach-only dial. Tests use it to
	// hold a retry until Disconnect or Close cancels it.
	onAttachDial func(context.Context)
	// beforeReconnectAdopt runs after a retry has a session and before it is
	// published. Disconnect or Close from the hook must discard that session.
	beforeReconnectAdopt func()
)

type Manager struct {
	mu             sync.Mutex
	store          *Store
	state          ConnectionState
	session        *Session
	cancel         context.CancelFunc
	closed         bool
	recovery       bool
	generation     uint64
	retired        map[string]struct{}
	pinned         HostProfile
	pinnedInstance string
	workers        sync.WaitGroup
}

func NewManager(store *Store) *Manager {
	return &Manager{store: store, state: ConnectionState{State: "disconnected"}, recovery: true, retired: map[string]struct{}{}}
}

// SetRecovery arms or disarms the single reconnect supervisor. Desktop
// candidates stay disarmed until Publish; a dropped candidate must not dial again.
func (m *Manager) SetRecovery(enabled bool) {
	m.mu.Lock()
	m.recovery = enabled
	m.mu.Unlock()
}

// Publish arms recovery only while this manager is still on the connected
// generation. A session that already ended is not resurrected.
func (m *Manager) Publish() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.state.State != "connected" || m.session == nil || m.session.Context.Err() != nil {
		return failure("connection_changed", "连接已断开或切换，请重新连接后重试")
	}
	m.recovery = true
	return nil
}

func cloneProfile(p HostProfile) HostProfile {
	if p.HostKey != nil {
		key := *p.HostKey
		p.HostKey = &key
	}
	return p
}

func sameProfile(a, b HostProfile) bool {
	if a.ID != b.ID || a.Name != b.Name || a.Host != b.Host || a.Port != b.Port || a.User != b.User || a.Auth != b.Auth || a.Executable != b.Executable || a.ConfigPath != b.ConfigPath {
		return false
	}
	if a.HostKey == nil || b.HostKey == nil {
		return false
	}
	return a.HostKey.Algorithm == b.HostKey.Algorithm && a.HostKey.Fingerprint == b.HostKey.Fingerprint
}

func cloneState(state ConnectionState) ConnectionState {
	if state.HostKey != nil {
		key := *state.HostKey
		state.HostKey = &key
	}
	if state.Manifest != nil {
		manifest := *state.Manifest
		manifest.Capabilities = maps.Clone(manifest.Capabilities)
		state.Manifest = &manifest
	}
	return state
}

func (m *Manager) Snapshot() ConnectionState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneState(m.state)
}

func (m *Manager) retire(id string) {
	if id == "" {
		return
	}
	if m.retired == nil {
		m.retired = map[string]struct{}{}
	}
	m.retired[id] = struct{}{}
}

// Connect starts asynchronously. The caller can poll Snapshot. A previous
// connection must be disconnected first; failure never switches host silently.
func (m *Manager) Connect(hostID string, secrets ConnectSecrets) (ConnectionState, error) {
	p, err := m.store.Get(hostID)
	if err != nil {
		return ConnectionState{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ConnectionState{}, failure("manager_closed", "远程连接服务已关闭")
	}
	switch m.state.State {
	case "connecting", "connected", "reconnecting":
		return cloneState(m.state), failure("connection_active", "请先断开当前连接，再连接其他主机")
	}
	if m.cancel != nil {
		m.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.generation++
	generation := m.generation
	id := newID()
	m.state = ConnectionState{State: "connecting", ConnectionID: id, HostID: p.ID, Phase: "ssh_connect", Message: "正在连接远程主机"}
	private := ConnectSecrets{Deploy: secrets.Deploy, Passphrase: append([]byte(nil), secrets.Passphrase...)}
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		m.supervise(ctx, generation, p, id, private)
	}()
	return cloneState(m.state), nil
}

func (m *Manager) setPhase(generation uint64, id, phase string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generation != generation || m.state.ConnectionID != id {
		return
	}
	if m.state.State == "connecting" || m.state.State == "reconnecting" {
		m.state.Phase = phase
	}
}

func (m *Manager) supervise(ctx context.Context, generation uint64, profile HostProfile, id string, secrets ConnectSecrets) {
	defer clear(secrets.Passphrase)
	session, err := connect(ctx, profile, secrets, func(phase string) { m.setPhase(generation, id, phase) })
	clear(secrets.Passphrase)
	secrets.Passphrase = nil
	secrets.Deploy = false
	if !m.finishInitial(ctx, generation, id, profile, session, err) {
		if session != nil {
			session.Close()
		}
		return
	}
	for {
		<-session.Context.Done()
		_ = session.Close()
		if !m.beginReconnect(generation, id, session.Reason()) {
			return
		}
		next, nextID, ok := m.retry(ctx, generation, id)
		if !ok {
			return
		}
		session = next
		id = nextID
	}
}

func (m *Manager) finishInitial(ctx context.Context, generation uint64, id string, profile HostProfile, session *Session, err error) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generation != generation || m.state.ConnectionID != id || m.state.State != "connecting" || ctx.Err() != nil {
		return false
	}
	if err != nil {
		m.state.State = "failed"
		m.state.Message = "远程连接未完成"
		m.state.ErrorCode = "connection_failed"
		var structured *Error
		if errors.As(err, &structured) {
			m.state.ErrorCode = structured.Code
			m.state.Message = structured.Message
			m.state.HostKey = cloneHostKey(structured.HostKey)
		}
		if m.cancel != nil {
			m.cancel()
			m.cancel = nil
		}
		return false
	}
	session.Ref = ConnectionRef{ID: id, HostID: profile.ID, InstanceID: session.Manifest.InstanceID}
	m.session = session
	manifest := session.Manifest
	m.state = ConnectionState{State: "connected", ConnectionID: id, HostID: profile.ID, InstanceID: manifest.InstanceID, Message: "已连接", Manifest: &manifest}
	m.pinned = cloneProfile(profile)
	m.pinnedInstance = manifest.InstanceID
	return true
}

func cloneHostKey(key *HostKey) *HostKey {
	if key == nil {
		return nil
	}
	copyKey := *key
	return &copyKey
}

func (m *Manager) beginReconnect(generation uint64, id, reason string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generation != generation || m.closed || m.state.ConnectionID != id || m.state.State != "connected" {
		return false
	}
	m.session = nil
	m.retire(id)
	if reason == "remote_session_expired" || !m.recovery {
		m.state.State = "failed"
		m.state.Phase = ""
		if reason == "remote_session_expired" {
			m.state.ErrorCode = "remote_session_expired"
			m.state.Message = "云端会话已失效，请重新连接并核对云端任务。不会自动重新登录。"
		} else {
			m.state.ErrorCode = "connection_lost"
			m.state.Message = "远程连接已中断，请重新连接并核对云端任务"
		}
		return false
	}
	m.state.State = "reconnecting"
	m.state.Phase = "ssh_reconnect"
	m.state.ErrorCode = ""
	m.state.HostKey = nil
	m.state.Message = "正在重新连接，已中断的操作不会自动重试"
	return true
}

func reconnectClass(code string) (state, message string, terminal bool) {
	switch code {
	case "passphrase_required":
		return "needs_auth", "重新连接需要重新输入密钥口令，口令不会被保存。", true
	case "ssh_auth_failed", "identity_invalid", "identity_permissions", "identity_read", "agent_unavailable":
		return "needs_auth", "SSH 认证未通过，已停止自动重连。请重新连接并确认密钥。", true
	case "remote_session_expired":
		return "failed", "云端会话已失效，请重新连接并核对云端任务。不会自动重新登录。", true
	case "server_session_failed":
		return "failed", "云端未接受新的连接票据。不会绕过登录或锁定状态自动重试。", true
	case "host_key_changed":
		return "failed", "SSH 主机密钥已变化，请核对主机身份后更新指纹", true
	case "host_key_unknown":
		return "failed", "请核对并确认 SSH 主机密钥指纹后重新连接", true
	case "instance_changed":
		return "failed", "云端 Cage 实例已变化，已停止自动重连。请核对主机身份后重新连接。", true
	case "profile_changed":
		return "failed", "远程主机配置已变化，已停止自动重连。请核对后重新连接。", true
	case "deployment_required", "cloud_setup_failed", "payload_missing", "protocol_incompatible", "manifest_invalid", "origin_refused", "server_start_timeout", "server_entry_failed":
		return "failed", "自动重连不会部署或启动云端环境。请核对服务后重新连接。", true
	case "service_unavailable", "ssh_connect_failed", "ssh_timeout", "connection_failed", "remote_request_failed":
		return "", "", false
	default:
		return "failed", "远程连接未完成，已停止自动重连。", true
	}
}

func (m *Manager) fail(generation uint64, id, state, code, message string, key *HostKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generation != generation || m.closed || m.state.ConnectionID != id {
		return
	}
	if m.state.State != "reconnecting" && m.state.State != "connected" && m.state.State != "connecting" {
		return
	}
	m.session = nil
	m.state.State = state
	m.state.Phase = ""
	m.state.ErrorCode = code
	m.state.Message = message
	m.state.HostKey = cloneHostKey(key)
}

func (m *Manager) retry(ctx context.Context, generation uint64, id string) (*Session, string, bool) {
	m.mu.Lock()
	pinned := cloneProfile(m.pinned)
	instanceID := m.pinnedInstance
	m.mu.Unlock()
	if instanceID == "" || pinned.ID == "" {
		m.fail(generation, id, "failed", "connection_lost", "远程连接已中断，请重新连接并核对云端任务", nil)
		return nil, "", false
	}
	for attempt := 1; attempt <= reconnectLimit; attempt++ {
		if !m.sleep(ctx, generation, id, reconnectBackoff(attempt)) {
			return nil, "", false
		}
		current, err := m.store.Get(pinned.ID)
		if err != nil || !sameProfile(pinned, current) {
			code := "profile_changed"
			message := "远程主机配置已变化，已停止自动重连。请核对后重新连接。"
			if err == nil && current.HostKey != nil && pinned.HostKey != nil && (current.HostKey.Algorithm != pinned.HostKey.Algorithm || current.HostKey.Fingerprint != pinned.HostKey.Fingerprint) {
				code = "host_key_changed"
				message = "SSH 主机密钥已变化，请核对主机身份后更新指纹"
				m.fail(generation, id, "failed", code, message, current.HostKey)
				return nil, "", false
			}
			m.fail(generation, id, "failed", code, message, nil)
			return nil, "", false
		}
		session, err := connect(ctx, pinned, ConnectSecrets{AttachOnly: true, ExpectInstanceID: instanceID}, func(phase string) {
			m.setPhase(generation, id, phase)
		})
		if ctx.Err() != nil {
			if session != nil {
				session.Close()
			}
			return nil, "", false
		}
		if err != nil {
			var structured *Error
			code := "connection_failed"
			var key *HostKey
			if errors.As(err, &structured) {
				code = structured.Code
				key = structured.HostKey
			}
			state, message, terminal := reconnectClass(code)
			if terminal {
				if message == "" {
					message = "远程连接未完成，已停止自动重连。"
				}
				if structured != nil && (code == "host_key_changed" || code == "host_key_unknown") && structured.Message != "" {
					message = structured.Message
				}
				m.fail(generation, id, state, code, message, key)
				return nil, "", false
			}
			continue
		}
		if session.Manifest.InstanceID != instanceID {
			session.Close()
			m.fail(generation, id, "failed", "instance_changed", "云端 Cage 实例已变化，已停止自动重连。请核对主机身份后重新连接。", nil)
			return nil, "", false
		}
		if beforeReconnectAdopt != nil {
			beforeReconnectAdopt()
		}
		nextID := newID()
		if !m.adopt(ctx, generation, id, nextID, session) {
			session.Close()
			return nil, "", false
		}
		return session, nextID, true
	}
	m.fail(generation, id, "failed", "connection_lost", "多次重新连接未完成。云端环境未自动启动，请核对后重新连接。", nil)
	return nil, "", false
}

func (m *Manager) sleep(ctx context.Context, generation uint64, id string, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generation == generation && !m.closed && m.state.State == "reconnecting" && m.state.ConnectionID == id
}

func (m *Manager) adopt(ctx context.Context, generation uint64, previousID, nextID string, session *Session) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || ctx.Err() != nil || m.generation != generation || m.state.State != "reconnecting" || m.state.ConnectionID != previousID {
		return false
	}
	if _, retired := m.retired[nextID]; retired {
		return false
	}
	m.retire(previousID)
	session.Ref = ConnectionRef{ID: nextID, HostID: m.pinned.ID, InstanceID: session.Manifest.InstanceID}
	m.session = session
	manifest := session.Manifest
	m.state = ConnectionState{State: "connected", ConnectionID: nextID, HostID: m.pinned.ID, InstanceID: manifest.InstanceID, Message: "已重新连接", Manifest: &manifest}
	return true
}

// Acquire refuses obsolete connection generations. Every runtime request and
// native file operation must carry the ID captured by its originating view.
func (m *Manager) Acquire(connectionID string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if connectionID == "" {
		return nil, failure("connection_changed", "连接已断开或切换，请重新连接后重试")
	}
	if _, retired := m.retired[connectionID]; retired {
		return nil, failure("connection_retired", "此连接代次已永久失效")
	}
	if m.state.State != "connected" || m.state.ConnectionID != connectionID || m.session == nil || m.session.Context.Err() != nil {
		return nil, failure("connection_changed", "连接已断开或切换，请重新连接后重试")
	}
	return m.session, nil
}

// AcquireCurrent returns the live session after a transport recovery. Callers
// that pin a desktop view generation must not keep using a retired SSH id.
func (m *Manager) AcquireCurrent() (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.State != "connected" || m.state.ConnectionID == "" || m.session == nil || m.session.Context.Err() != nil {
		return nil, failure("connection_changed", "连接已断开或切换，请重新连接后重试")
	}
	if _, retired := m.retired[m.state.ConnectionID]; retired {
		return nil, failure("connection_retired", "此连接代次已永久失效")
	}
	return m.session, nil
}

func (m *Manager) Disconnect(connectionID string) error {
	m.mu.Lock()
	if m.state.State == "disconnected" {
		m.mu.Unlock()
		return nil
	}
	if connectionID == "" || m.state.ConnectionID != connectionID {
		m.mu.Unlock()
		return failure("connection_changed", "连接已切换，未断开其他连接")
	}
	m.generation++
	m.retire(connectionID)
	cancel, session := m.cancel, m.session
	m.cancel = nil
	m.session = nil
	m.state = ConnectionState{State: "disconnected"}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if session != nil {
		return session.Close()
	}
	return nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	m.generation++
	m.retire(m.state.ConnectionID)
	cancel, session := m.cancel, m.session
	m.cancel = nil
	m.session = nil
	m.state = ConnectionState{State: "disconnected"}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	var err error
	if session != nil {
		err = session.Close()
	}
	m.workers.Wait()
	return err
}
