package workbench

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime"
	"strings"
	"sync"

	"github.com/sylvinus/agent-vm/internal/remotehost"
)

const connectionHeader = "X-Cage-Connection"

type remoteGateway struct {
	mu           sync.Mutex
	remote       bool
	clientOnly   bool
	clientShares map[string]*clientProjectGrant
	store        *remotehost.Store
	manager      *remotehost.Manager
	desktop      *desktopGateway
}

func newRemoteGateway(profile string) *remoteGateway {
	store := remotehost.NewStore(profile)
	return &remoteGateway{store: store, manager: remotehost.NewManager(store)}
}

func (g *remoteGateway) close() {
	if g.desktop != nil {
		g.desktop.close()
	}
	_ = g.manager.Close()
	g.drainClosedClientShares()
}

func (g *remoteGateway) isRemote() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.remote
}

func (g *remoteGateway) setRemote(remote bool) {
	g.mu.Lock()
	g.remote = remote
	g.mu.Unlock()
}

type ClientContext struct {
	DesktopGateway  bool                       `json:"desktop_gateway,omitempty"`
	Candidate       *desktopCandidate          `json:"candidate,omitempty"`
	ClientOnly      bool                       `json:"client_only"`
	Mode            string                     `json:"mode"`
	ClientPlatform  string                     `json:"client_platform"`
	ConnectionID    string                     `json:"connection_id"`
	State           remotehost.ConnectionState `json:"connection"`
	InstanceID      string                     `json:"instance_id,omitempty"`
	HostID          string                     `json:"host_id,omitempty"`
	HostFingerprint string                     `json:"host_fingerprint,omitempty"`
	ServerPlatform  string                     `json:"server_platform"`
	Name            string                     `json:"name,omitempty"`
}

func (g *remoteGateway) context() ClientContext {
	if g.desktop != nil {
		return g.desktop.context()
	}
	result := ClientContext{Mode: "local", ConnectionID: "local", ClientPlatform: runtime.GOOS, ServerPlatform: runtime.GOOS}
	result.ClientOnly = g.clientOnly
	result.State = g.manager.Snapshot()
	if !g.isRemote() {
		return result
	}
	result.Mode, result.ConnectionID = "remote", result.State.ConnectionID
	result.HostID, result.InstanceID = result.State.HostID, result.State.InstanceID
	result.ServerPlatform = "linux"
	if hosts, err := g.store.List(); err == nil {
		for _, host := range hosts {
			if host.ID == result.HostID {
				result.Name = host.Name
				if host.HostKey != nil {
					result.HostFingerprint = host.HostKey.Fingerprint
				}
				break
			}
		}
	}
	return result
}

func (s *Server) clientRequest(w http.ResponseWriter, r *http.Request) {
	g := s.remote
	if g == nil {
		http.Error(w, "远程连接服务不可用", 503)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/client/")
	if path == "browser-extensions" && (r.Method == http.MethodGet || r.Method == http.MethodPost) {
		s.localBrowserExtensions(w, r)
		return
	}
	if path == "project-shares" || strings.HasPrefix(path, "project-shares/") {
		s.clientProjectShareRequest(w, r, path)
		return
	}
	if path == "commit" || path == "disconnect" || path == "local" {
		defer g.drainClosedClientShares()
	}
	if g.desktop != nil && g.desktop.handle(path, w, r) {
		return
	}
	switch {
	case r.Method == "GET" && path == "context":
		jsonReply(w, g.context())
	case r.Method == "GET" && path == "connection":
		jsonReply(w, g.context().State)
	case r.Method == "GET" && path == "hosts":
		hosts, err := g.store.List()
		if err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, map[string]any{"hosts": hosts})
	case (r.Method == "POST" && path == "hosts") || (r.Method == "PUT" && strings.HasPrefix(path, "hosts/")):
		var host remotehost.HostProfile
		if !readJSON(w, r, &host) {
			return
		}
		if r.Method == "PUT" {
			host.ID = strings.TrimPrefix(path, "hosts/")
		} else {
			host.ID = ""
		}
		if g.desktop != nil {
			g.desktop.change.Lock()
			defer g.desktop.change.Unlock()
		}
		if host.ID != "" && g.hostInUse(host.ID) {
			http.Error(w, "请先断开这台主机，再修改连接配置", 409)
			return
		}
		saved, err := g.store.Upsert(host)
		if err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, saved)
	case r.Method == "DELETE" && strings.HasPrefix(path, "hosts/"):
		id := strings.TrimPrefix(path, "hosts/")
		if g.desktop != nil {
			g.desktop.change.Lock()
			defer g.desktop.change.Unlock()
		}
		if g.hostInUse(id) {
			http.Error(w, "请先断开这台主机，再删除本机连接配置", 409)
			return
		}
		if err := g.store.Delete(id); err != nil {
			requestError(w, err)
			return
		}
		jsonReply(w, map[string]bool{"ok": true})
	case r.Method == "POST" && path == "connect":
		var input struct {
			HostID     string `json:"host_id"`
			Deploy     bool   `json:"deploy,omitempty"`
			Passphrase string `json:"passphrase"`
		}
		if !readJSON(w, r, &input) {
			return
		}
		secret := []byte(input.Passphrase)
		input.Passphrase = ""
		state, err := g.manager.Connect(input.HostID, remotehost.ConnectSecrets{Passphrase: secret, Deploy: input.Deploy})
		clear(secret)
		if err != nil {
			clear(secret)
			requestError(w, err)
			return
		}
		g.setRemote(true)
		w.WriteHeader(http.StatusAccepted)
		jsonReply(w, state)
	case r.Method == "POST" && (path == "disconnect" || path == "local"):
		if path == "local" && g.clientOnly {
			http.Error(w, "轻量客户端仅连接远程环境", 409)
			return
		}
		var input struct {
			ConnectionID string `json:"connection_id"`
		}
		if !readJSON(w, r, &input) {
			return
		}
		if err := g.manager.Disconnect(input.ConnectionID); err != nil {
			requestError(w, err)
			return
		}
		if path == "local" {
			g.setRemote(false)
		}
		jsonReply(w, g.context())
	default:
		http.NotFound(w, r)
	}
}

func (g *remoteGateway) hostInUse(id string) bool {
	c := g.context()
	inUse := func(state remotehost.ConnectionState) bool {
		switch state.State {
		case "connecting", "connected", "reconnecting":
			return state.HostID == id
		default:
			return false
		}
	}
	return inUse(c.State) || (c.Candidate != nil && inUse(c.Candidate.Connection))
}

func requestConnection(r *http.Request) string {
	if value := r.Header.Get(connectionHeader); value != "" {
		return value
	}
	return r.URL.Query().Get("connection")
}

// routeRemote runs after local Host/Origin/session authentication and before
// the local account gate. Only business API requests cross SSH: neither the
// remote server's scripts nor its bootstrap/update routes enter the client.
func (s *Server) routeRemote(w http.ResponseWriter, r *http.Request) bool {
	if s.remote == nil || !strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	if strings.HasPrefix(r.URL.Path, "/api/client/") {
		s.clientRequest(w, r)
		return true
	}
	switch r.URL.Path {
	case "/api/session", "/api/launch", "/api/handoff":
		return false
	case "/api/remote/manifest":
		if s.remote.desktop != nil {
			http.NotFound(w, r)
			return true
		}
		return false
	}
	if r.URL.Path == "/api/diagnostics/events" {
		return false
	}
	var session *remotehost.Session
	remote := true
	var err error
	if desktop := s.remote.desktop; desktop != nil {
		session, remote, err = desktop.acquire(requestConnection(r))
	} else if !s.remote.isRemote() {
		if s.clientOnly && r.URL.Path != "/api/diagnostics/events" {
			http.Error(w, "请先连接远程主机", 409)
			return true
		}
		if id := requestConnection(r); id != "" && id != "local" {
			http.Error(w, "连接已经切换，请重新打开当前工作台", 409)
			return true
		}
		return false
	} else {
		session, err = s.remote.manager.Acquire(requestConnection(r))
	}
	if remote && r.URL.Path == "/api/projects/client-share" {
		http.NotFound(w, r)
		return true
	}
	if remote && (r.URL.Path == "/api/environment/storage" || r.URL.Path == "/api/environment/storage/recover") {
		http.Error(w, "请在虚拟机所在电脑上调整环境存储", http.StatusForbidden)
		return true
	}
	if err != nil {
		http.Error(w, "连接不可用或已切换，请重新选择工作环境", 409)
		return true
	}
	if r.URL.Path == "/api/diagnostics/export" {
		if !s.diagnosticMu.TryLock() {
			http.Error(w, "正在导出另一份诊断包，请稍候", http.StatusConflict)
			return true
		}
		defer s.diagnosticMu.Unlock()
	}
	if remote && strings.HasPrefix(r.URL.Path, "/api/mobile/") {
		http.Error(w, "手机只连接当前电脑的 Cage 环境", http.StatusForbidden)
		return true
	}
	if remote && (r.URL.Path == "/api/mobile-pair" || strings.HasPrefix(r.URL.Path, "/api/mobile-pair/")) {
		http.Error(w, "请切换到这台电脑的本机环境，再管理手机配对", http.StatusForbidden)
		return true
	}
	if remote && r.Method == "GET" && r.URL.Path == "/api/browser/files/saved" {
		profile, err := clientDownloadProfile(s.profile, ClientContext{Mode: "remote", InstanceID: session.Ref.InstanceID})
		if err != nil {
			requestError(w, err)
			return true
		}
		serveSavedDownloads(w, profile)
		return true
	}
	target, err := url.Parse(session.Origin)
	if err != nil {
		http.Error(w, "远程入口无效", 502)
		return true
	}
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(session.Context, cancel)
	defer func() { stop(); cancel() }()
	proxy := &httputil.ReverseProxy{
		Transport:     session.Transport,
		FlushInterval: -1,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(target)
			p.Out.Host = target.Host
			p.Out.Header.Del(connectionHeader)
			query := p.Out.URL.Query()
			query.Del("connection")
			p.Out.URL.RawQuery = query.Encode()
		},
		ModifyResponse: func(response *http.Response) error {
			response.Header.Del("Set-Cookie")
			response.Header.Del("Content-Security-Policy")
			response.Header.Del("Access-Control-Allow-Origin")
			response.Header.Set("X-Content-Type-Options", "nosniff")
			if response.StatusCode == http.StatusOK && response.Request.URL.Path == "/api/diagnostics/export" && strings.HasPrefix(response.Header.Get("Content-Type"), "application/zip") {
				return s.addDesktopDiagnosticResponse(response)
			}
			if response.StatusCode >= 300 && response.StatusCode < 400 {
				return errors.New("remote redirects are refused")
			}
			if downloadID := strings.TrimSuffix(strings.TrimPrefix(response.Request.URL.Path, "/api/browser/files/downloads/"), "/content"); strings.HasSuffix(response.Request.URL.Path, "/content") && browserFileID.MatchString(downloadID) {
				name := "download"
				_, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
				if err == nil && browserDownloadName(parameters["filename"]) {
					name = parameters["filename"]
				}
				response.Header.Set("Content-Type", "application/octet-stream")
				response.Header.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
				return nil
			}
			if response.StatusCode != http.StatusSwitchingProtocols && response.StatusCode != http.StatusNoContent {
				contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
				if err != nil {
					return errors.New("remote response has no supported content type")
				}
				switch contentType {
				case "application/json", "text/plain", "text/event-stream", "application/octet-stream", "application/zip", "image/png", "image/jpeg", "image/webp":
				default:
					return errors.New("remote active content is refused")
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "远程连接已中断；已提交的操作请重连后核对", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r.WithContext(ctx))
	return true
}
