package workbench

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/guestos"
	"github.com/sylvinus/agent-vm/internal/host"
	"github.com/sylvinus/agent-vm/internal/hostimage"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/*
var assets embed.FS

const cookieName = "cage_session"

type controlFile struct {
	Port       int                `json:"port"`
	Token      string             `json:"token"`
	PID        int                `json:"pid"`
	Controller ControllerIdentity `json:"controller"`
}
type Server struct {
	desktopViews                                     displayViewDemand
	desktopDisplayUpdate                             func(bool) error
	browserViews                                     displayViewDemand
	desktopStatus                                    desktopStatusCache
	agentNativeQueue                                 agentNativeQueue
	macSetupProjectsChecked                          time.Time
	macSetupProjectsAttempt                          time.Time
	macProjectsRetryAt                               time.Time
	macLoginLease                                    *macLoginLease
	macosGuest                                       *guestos.MacOSProfile
	macResources                                     macOSResourceObserver
	windowsGuest                                     *guestos.WindowsProfile
	windowsState                                     windowsGuestState
	windowsProjectHistory                            map[string]string
	clipboardFiles                                   map[string]clipboardFileSelection
	clipboardRelease                                 func([]string)
	clipboardCancel                                  context.CancelFunc
	clipboardGuestSession                            string
	phones                                           mobilePairing
	instanceID                                       string
	clientOnly                                       bool
	desktopEntry                                     bool
	windowOwners                                     map[int]bool
	remote                                           *remoteGateway
	account                                          *localAccount
	identity                                         ControllerIdentity
	guestManifest                                    *cage.GuestManifest
	guestUpdateRequired                              bool
	guestUpdateError                                 string
	storageRecoveryError                             string
	closing                                          bool
	shutdown                                         func()
	claudeAccounts                                   claudeAccountManager
	favoritesMu                                      sync.Mutex
	diagnosticMu                                     sync.Mutex
	diagnosticRateMu                                 sync.Mutex
	diagnosticWindow                                 time.Time
	diagnosticCount                                  int
	tickets                                          map[string]time.Time
	browserControl                                   *browserClient
	controlSession                                   guestSession
	windowsBrowserControl                            *windowsBrowserClient
	nativeDownloads                                  map[string]*nativeDownload
	nativeDownloadSlots                              chan struct{}
	nativeDownloadRevision                           uint64
	windowsNetwork                                   *windowsHostNetwork
	op                                               sync.Mutex
	profile, configPath, exe, origin, token, session string
	mu                                               sync.Mutex
	preferences                                      Preferences
	jobs                                             jobManager
	ready                                            bool
	location                                         *Location
	exitProbeError                                   string
	locationError                                    string
	networkIntentError                               string
	credentials                                      guestCredentials
	terminals                                        terminalManager
	mobileCLI                                        mobileCLIStore
	conversationMu                                   sync.Mutex
	mobileConnectMu                                  sync.Mutex
	audioClients                                     int
	projectShares                                    map[string]*projectShare
	projectShareEpoch                                map[string]uint64
	projectRecordsMu                                 sync.Mutex
	projectErrors                                    []projectShareError
	clientMounts                                     map[string]*guestClientMount
	clientWorkspace                                  string
	safety                                           safetyState
	recovery                                         networkRecovery
	locations                                        map[string]Location
	monitorCancel                                    context.CancelFunc
	monitorSafetyOp                                  sync.Mutex
	safetyOp                                         sync.Mutex
	networkPaused                                    bool
	proxyNetworkLost                                 bool
	checkCancel                                      context.CancelFunc
	lifeContext                                      context.Context
	lifeCancel                                       context.CancelFunc
	monitors                                         sync.WaitGroup
	activities                                       sync.WaitGroup
	closeOnce                                        sync.Once
	closeErr                                         error
	controlPublished                                 bool
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func equalSecret(a, b string) bool {
	return len(a) > 0 && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func Serve(configPath string) error { return serve(configPath, false, "") }

func ServeClient(configPath string) error { return serve(configPath, true, "") }

// ServeDesktop is the desktop window's connection gateway. Its control lock and
// logs live separately from the workload it may later attach to.
func ServeDesktop(fullConfigPath string) error {
	return serve(desktopConfigPath(fullConfigPath), false, fullConfigPath)
}

func serve(configPath string, clientOnly bool, fullConfigPath string) (returned error) {
	store := diagnostics.New(filepath.Dir(configPath))
	diagnostics.SetDefault(store)
	if err := store.Clean(); err != nil {
		return err
	}

	diagnostics.Log("info", "controller", "service starting")
	defer func() { diagnostics.Log("info", "controller", "service stopped") }()
	profile := filepath.Dir(configPath)
	if err := host.EnsurePrivateDir(profile); err != nil {
		return err
	}
	lock, err := openControllerLock(profile, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = lockControllerForStart(lock); err != nil {
		if controllerLockContended(err) {
			return errors.New("此配置已有工作台服务正在运行")
		}
		return fmt.Errorf("无法锁定工作台控制文件: %w", err)
	}
	if err := host.ProtectFile(lock.Name()); err != nil {
		return err
	}
	if err := store.ImportLegacy(filepath.Dir(configPath)); err != nil {
		diagnostics.Log("warn", "diagnostics", "legacy log import failed: "+err.Error())
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	var listener net.Listener
	port := 17777
	for ; port < 17808; port++ {
		listener, err = net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			break
		}
	}
	if err != nil {
		return err
	}
	defer listener.Close()
	s := &Server{profile: profile, configPath: configPath, exe: exe, clientOnly: clientOnly, origin: fmt.Sprintf("http://localhost:%d", port), token: randomToken(), session: randomToken(), preferences: Preferences{Keyboard: defaultKeyboard(), AutoTimezone: true, AutoLanguage: true, BalancedDisplay: true}}
	s.desktopEntry = fullConfigPath != ""
	workload := !s.desktopEntry && !clientOnly
	if s.desktopEntry {
		if err = importDesktopConnections(filepath.Dir(fullConfigPath), profile); err != nil {
			return err
		}
		s.remote = newDesktopGateway(profile, NewDesktopLocalController(fullConfigPath, exe))
	} else {
		s.remote = newRemoteGateway(profile)
		s.remote.clientOnly = clientOnly
	}
	defer s.remote.close()
	if workload {
		if err = guestos.RecoverWindowsStorage(context.Background(), profile); err != nil {
			s.failStorageRecovery(err)
		}
		s.macosGuest, err = guestos.LoadMacOSProfile(profile)
		if err != nil {
			return err
		}
		s.windowsGuest, err = guestos.LoadWindowsProfile(profile)
		if err != nil {
			return err
		}
		if s.macosGuest != nil && s.windowsGuest != nil {
			return errors.New("一个配置只能绑定一个客机系统")
		}
		if s.windowsGuest != nil {
			pending, err := guestos.WindowsCloneCleanupPending(profile)
			if err != nil {
				return err
			}
			if pending {
				s.windowsGuest = nil
			}
		}
		s.instanceID, err = ensureRemoteInstance(profile)
		if err != nil {
			return err
		}
		s.account, err = loadLocalAccount(profile)
		if err != nil {
			return err
		}
		defer s.account.lock()
		defer func() { returned = errors.Join(returned, s.closeService()) }()
		if b, e := os.ReadFile(filepath.Join(profile, "workbench.json")); e == nil {
			// Existing profiles without this field used manual language settings.
			s.preferences.AutoLanguage = false
			// Preserve the original density of profiles saved before this setting.
			s.preferences.BalancedDisplay = false
			_ = json.Unmarshal(b, &s.preferences)
		}
		if !s.storageRecoveryStatus().Required {
			if err = s.prepareEnvironment(); err != nil {
				if s.windowsGuest == nil {
					return err
				}
				s.failStorageRecovery(err)
			}
		}
		// Invalid intent keeps the gate paused and its error visible in status.
		s.restoreNetworkIntent()
		if s.preferences.Keyboard == "" {
			s.preferences.Keyboard = defaultKeyboard()
		}
		if _, e := os.Stat(configPath); os.IsNotExist(e) {
			if err = writePrivate(configPath, defaultConfig()); err != nil {
				return err
			}
		}
		// A previously running VM cannot keep an unmanaged endpoint online.
		if s.macosGuest != nil {
			_ = s.closeProxyGate("控制服务已重新连接，外网等待重新检查")
		} else if cfg, e := loadConfig(configPath); s.windowsGuest == nil && (e != nil || cfg.RequireManagedProxy(profile) != nil) {
			_ = s.closeProxyGate("尚未配置有效节点，外网保持关闭")
		}
		if s.networkPaused && s.windowsGuest == nil {
			_ = s.closeProxyGate("网络保持暂停，请手动恢复后再联网")
		}
		s.startMonitors()
		if err = s.restorePhone(); err != nil {
			s.phones.status = "无法恢复手机配对，请移除旧记录后重试"
		}
		defer s.stopPhone()
	}
	s.identity, err = currentController()
	if err != nil {
		return err
	}
	if err = writePrivate(filepath.Join(profile, "control.json"), controlFile{Port: port, Token: s.token, PID: os.Getpid(), Controller: s.identity}); err != nil {
		return err
	}
	s.controlPublished = true
	if !workload {
		defer os.Remove(filepath.Join(profile, "control.json"))
	}
	serverErrors := store.Stream("http", "error")
	defer serverErrors.Close()
	server := &http.Server{ErrorLog: log.New(serverErrors, "", 0), Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	defer server.Close()
	shutdownDone := make(chan struct{})
	var shutdownOnce sync.Once
	s.shutdown = func() {
		shutdownOnce.Do(func() {
			defer close(shutdownDone)
			s.beginClose()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = server.Shutdown(ctx)
			_ = server.Close()
		})
	}
	signals, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	serveDone := make(chan struct{})
	defer close(serveDone)
	if s.desktopEntry {
		go s.watchDesktopOwners(serveDone)
	}
	go func() {
		select {
		case <-signals.Done():
			s.shutdown()
		case <-serveDone:
		}
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}
func (s *Server) handler() http.Handler {
	mux := newDiscoverableMux()
	mux.HandleFunc("GET /api/agent/routes", func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, mux.routes())
	})
	s.agentRoutes(mux)
	s.agentNativeRoutes(mux)
	s.mobileCLIRoutes(mux)
	s.mobileSurfaceRoutes(mux)
	mux.HandleFunc("GET /api/mobile-pair", s.phoneStatus)
	mux.HandleFunc("POST /api/mobile-pair", s.pairPhone)
	mux.HandleFunc("DELETE /api/mobile-pair", s.removePhone)
	mux.HandleFunc("POST /api/mobile/device", s.reportPhoneDevice)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { jsonReply(w, map[string]bool{"ready": true}) })
	mux.HandleFunc("POST /api/launch", func(w http.ResponseWriter, r *http.Request) {
		if !s.launcherAuthenticated(w, r) {
			return
		}
		jsonReply(w, launchResponse{ConfigPath: s.configPath, ClientOnly: s.clientOnly, DesktopGateway: s.desktopEntry, URL: s.origin + "/?launch=" + randomToken()[:8] + "#" + s.issueTicket(), Controller: s.identity})
	})
	if s.desktopEntry || s.clientOnly {
		mux.HandleFunc("POST /api/handoff", s.handoffEntry)
	} else {
		mux.HandleFunc("POST /api/handoff", s.handoff)
		mux.HandleFunc("POST /api/desktop-local/trusted-login", s.localTrustedLogin)
		mux.HandleFunc("POST /api/desktop-local/{action}", s.desktopLocalControl)
	}
	if s.desktopEntry {
		mux.HandleFunc("POST /api/desktop-owner", s.registerDesktopOwner)
	}
	mux.HandleFunc("POST /api/session", func(w http.ResponseWriter, r *http.Request) {
		var v struct {
			Token string `json:"token"`
		}
		if !readJSON(w, r, &v) {
			return
		}
		if !s.consumeTicket(v.Token) {
			http.Error(w, "访问凭证无效，请从 Cage 应用重新打开。", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: s.session, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 86400 * 30})
		jsonReply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/environment/initialization", s.initialization)
	mux.HandleFunc("POST /api/environment/initialization", s.initialization)
	mux.HandleFunc("GET /api/remote/manifest", s.remoteManifest)
	mux.HandleFunc("GET /api/projects/directories", s.projectDirectories)
	mux.HandleFunc("POST /api/input", s.remoteInput)
	mux.HandleFunc("GET /api/diagnostics", s.diagnosticStatus)
	mux.HandleFunc("POST /api/diagnostics/events", s.diagnosticEvent)
	mux.HandleFunc("GET /api/diagnostics/export", s.diagnosticExport)
	mux.HandleFunc("GET /api/claude-accounts", s.listClaudeAccounts)
	mux.HandleFunc("POST /api/claude-accounts", s.mutateClaudeAccounts)
	mux.HandleFunc("GET /api/vault", s.vaultList)
	mux.HandleFunc("POST /api/vault", s.vaultMutation)
	mux.HandleFunc("POST /api/vault/browser", s.vaultBrowser)
	mux.HandleFunc("GET /api/account", s.accountStatus)
	mux.HandleFunc("POST /api/account/{action}", s.accountAction)
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("POST /api/projects/open", s.openProject)
	mux.HandleFunc("POST /api/projects/remove", s.removeProject)
	mux.HandleFunc("POST /api/projects/client-share", s.guestClientShare)
	mux.HandleFunc("GET /api/projects/client-share", s.guestClientShare)
	mux.HandleFunc("GET /api/environment/versions", s.environmentVersions)
	mux.HandleFunc("GET /api/environment/guest-systems", s.guestInstallation)
	mux.HandleFunc("GET /api/environment/guest-systems/clone", s.hostClone)
	mux.HandleFunc("DELETE /api/environment/guest-systems", s.guestInstallation)
	mux.HandleFunc("POST /api/environment/guest-systems/activate", s.guestInstallation)
	mux.HandleFunc("GET /api/environment/storage", s.environmentStorage)
	mux.HandleFunc("POST /api/environment/storage", s.environmentStorage)
	mux.HandleFunc("POST /api/environment/storage/recover", s.recoverEnvironmentStorage)
	mux.HandleFunc("GET /api/environment/resources", s.resourceStatus)
	mux.HandleFunc("POST /api/environment/resources", s.applyResources)
	mux.HandleFunc("POST /api/environment/update", s.updateEnvironment)
	mux.HandleFunc("POST /api/environment/tools", s.updateEnvironmentTools)
	mux.HandleFunc("GET /api/host/git", s.hostGit)
	mux.HandleFunc("POST /api/host/git", s.enableHostGit)
	mux.HandleFunc("GET /api/appearance", s.getAppearance)
	mux.HandleFunc("GET /api/favorites", s.serveFavorites)
	mux.HandleFunc("POST /api/favorites", s.serveFavorites)
	mux.HandleFunc("POST /api/appearance", s.saveAppearance)
	mux.HandleFunc("GET /api/appearance/codex", s.codexAppearance)
	mux.HandleFunc("POST /api/settings/probe", s.probe)
	mux.HandleFunc("GET /api/proxy", s.proxyStatus)
	mux.HandleFunc("POST /api/proxy/delay", s.proxyDelay)
	mux.HandleFunc("GET /api/proxy/connections", s.proxyConnections)
	mux.HandleFunc("GET /api/proxy/logs", s.proxyLogs)
	mux.HandleFunc("POST /api/proxy/{action}", s.proxyMutation)
	mux.HandleFunc("POST /api/settings", s.saveSettings)
	mux.HandleFunc("POST /api/restore", s.restore)
	mux.HandleFunc("POST /api/start", s.start)
	mux.HandleFunc("POST /api/stop", s.stop)
	mux.HandleFunc("POST /api/safety/check", s.safetyCheck)
	mux.HandleFunc("POST /api/safety/close", s.pauseNetwork)
	mux.HandleFunc("GET /api/safety/report", s.safetyExport)
	mux.HandleFunc("GET /api/tailscale", s.tailnet)
	mux.HandleFunc("POST /api/tailscale/{action}", s.tailnet)
	mux.HandleFunc("GET /api/browser/tabs", s.browser)
	mux.HandleFunc("GET /api/browser/events", s.browserEvents)
	mux.HandleFunc("POST /api/browser/clipboard", s.browserClipboard)
	mux.HandleFunc("GET /api/browser/preferences", s.browserPreferences)
	mux.HandleFunc("POST /api/browser/preferences", s.browserPreferences)
	mux.HandleFunc("POST /api/browser/extensions/upload", s.browserExtensionUpload)
	mux.HandleFunc("GET /api/client/browser-extensions", s.localBrowserExtensions)
	mux.HandleFunc("POST /api/client/browser-extensions", s.localBrowserExtensions)
	mux.HandleFunc("POST /api/browser/{action}", s.browser)
	mux.HandleFunc("POST /api/input/image", s.imageInput)
	mux.HandleFunc("POST /api/input/text", s.textInput)
	mux.HandleFunc("POST /api/input/paste/start", s.pasteInputStart)
	mux.HandleFunc("POST /api/input/paste/cancel", s.pasteInputCancel)
	mux.HandleFunc("POST /api/input/files/{action}", s.fileInputAction)
	mux.HandleFunc("POST /api/input/files/{target}/{id}", s.fileInputUpload)
	mux.HandleFunc("GET /api/input/files/desktop/requests", s.desktopFileRequests)
	mux.HandleFunc("POST /api/input/files/desktop/open", s.desktopFileOpenAction)
	mux.HandleFunc("POST /api/browser/files/uploads/{id}", s.browserUpload)
	mux.HandleFunc("GET /api/browser/files/downloads/{id}", s.browserDownload)
	mux.HandleFunc("GET /api/browser/files/downloads/{id}/{action}", s.browserDownload)
	mux.HandleFunc("GET /api/browser/files/saved", s.savedBrowserDownloads)
	mux.HandleFunc("GET /api/vnc-credentials", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.ready {
			http.Error(w, "环境尚未就绪", 409)
			return
		}
		jsonReply(w, map[string]any{"password": s.credentials.VNCPassword, "display_scale": s.credentials.DisplayScale, "dynamic_scale": s.credentials.DynamicScale})
	})
	mux.HandleFunc("GET /api/vnc", s.vnc)
	mux.HandleFunc("GET /api/claude-desktop", s.claudeDesktopStatus)
	mux.HandleFunc("GET /api/claude-desktop/vnc-credentials", s.claudeDesktopCredentials)
	mux.HandleFunc("GET /api/claude-desktop/vnc", s.claudeDesktopVNC)
	mux.HandleFunc("POST /api/claude-desktop/resize", s.claudeDesktopResize)
	mux.HandleFunc("POST /api/claude-desktop/{action}", s.claudeDesktopAction)
	if s.macosGuest != nil {
		mux.HandleFunc("POST /api/macos/first-setup", s.macOSLoginWindow)
		mux.HandleFunc("POST /api/macos/login", s.macOSLoginWindow)
		mux.HandleFunc("POST /api/macos/maintenance", s.macOSLoginWindow)
		s.registerMacOSClaudeRoutes(mux)
		s.registerMacOSBrowserRoutes(mux)
		mux.HandleFunc("POST /api/macos/project-approval", s.macOSProjectApproval)
		mux.HandleFunc("POST /api/macos/exit-policy", s.bindMacOSExit)
	}
	mux.HandleFunc("GET /api/terminals", s.listTerminals)
	mux.HandleFunc("GET /api/audio", s.audio)
	mux.HandleFunc("GET /api/clipboard", s.sharedClipboard)
	mux.HandleFunc("POST /api/clipboard/copy", s.clipboardCopy)
	mux.HandleFunc("POST /api/clipboard/files", s.clipboardArchive)
	mux.HandleFunc("GET /api/clipboard/files/{id}", s.clipboardArchive)
	mux.HandleFunc("DELETE /api/clipboard/files/{id}", s.clipboardArchive)
	mux.HandleFunc("POST /api/terminals", s.newTerminal)
	mux.HandleFunc("DELETE /api/terminals/{id}", s.closeTerminal)
	mux.HandleFunc("POST /api/terminals/{id}/login-link", s.terminalLoginLink)
	mux.HandleFunc("POST /api/terminals/{id}/metadata", s.terminalSessionMetadata)
	mux.HandleFunc("GET /api/terminals/{id}/stream", s.terminalStream)
	web, _ := fs.Sub(assets, "web")
	mux.Handle("GET /", http.FileServer(http.FS(web)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin, err := url.Parse(s.origin)
		if err != nil || r.Host != origin.Host {
			http.Error(w, "Host refused", 403)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' ws://"+origin.Host+"; img-src 'self' data: blob:; media-src 'self' blob:; font-src 'self'; object-src 'none'; frame-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		if (r.Method != "GET" && r.Method != "HEAD") || strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			if r.Header.Get("Origin") != s.origin {
				http.Error(w, "Origin refused", 403)
				return
			}
		}
		// Launcher-token routes are handled only by this listener, never by a
		// selected workload. They perform their own authentication.
		if strings.HasPrefix(r.URL.Path, "/api/desktop-local/") || r.URL.Path == "/api/desktop-owner" {
			if (strings.HasPrefix(r.URL.Path, "/api/desktop-local/") && (s.desktopEntry || s.clientOnly)) || (r.URL.Path == "/api/desktop-owner" && !s.desktopEntry) {
				http.NotFound(w, r)
				return
			}
			mux.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/remote/manifest" && (s.desktopEntry || s.clientOnly) {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/session" && r.URL.Path != "/api/launch" && r.URL.Path != "/api/handoff" {
			c, e := r.Cookie(s.cookieName())
			if e != nil || !equalSecret(c.Value, s.session) {
				http.Error(w, "请从 Cage 应用重新打开工作台", 401)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/agent/native") || r.URL.Path == "/api/agent/routes" {
			mux.ServeHTTP(w, r)
			return
		}
		if s.windowsGuest != nil && !windowsWorkbenchRoute(r.URL.Path) {
			s.windowsUnavailable(w)
			return
		}
		if s.macosGuest != nil && (!macOSWorkbenchRoute(r.URL.Path) || s.macSetupPending() && !macOSSetupRoute(r.URL.Path)) {
			http.Error(w, "此功能尚未接入 macOS 客机", http.StatusConflict)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/mobile/") {
			// Device trust is scoped to this host. Never forward a paired phone
			// into a different host selected by the desktop SSH client.
			if s.desktopEntry && !s.clientOnly && s.remote != nil && s.remote.desktop != nil && r.Header.Get(connectionHeader) != "" {
				// The desktop's own viewer pins the selected local workload's
				// connection generation. routeRemote checks the acquired target.
				if s.routeRemote(w, r) {
					return
				}
			}
			if s.clientOnly || s.desktopEntry {
				http.Error(w, "请连接运行 Cage 环境的主机", http.StatusConflict)
				return
			}
		} else if s.macosGuest == nil && s.windowsGuest == nil && s.routeRemote(w, r) {
			return
		}
		ctx, finish, err := s.beginActivity(r.Context())
		if err != nil {
			http.Error(w, "后台正在更新，请重新打开 Cage。", http.StatusServiceUnavailable)
			return
		}
		defer finish()
		r = r.WithContext(ctx)
		accountRoute := r.URL.Path == "/api/account" || strings.HasPrefix(r.URL.Path, "/api/account/")
		bootstrapRoute := r.URL.Path == "/api/session" || r.URL.Path == "/api/launch" || r.URL.Path == "/api/handoff" || r.URL.Path == "/api/remote/manifest"
		var phoneRoom context.Context
		if (s.windowsGuest != nil || s.macosGuest != nil) && strings.HasPrefix(r.URL.Path, "/api/") && !bootstrapRoute && !s.hasTrustedView(r) {
			phoneRoom = s.nativePhoneRoom(r)
			if phoneRoom == nil {
				http.Error(w, "请通过本机 Cage 窗口或当前配对手机打开客机", http.StatusForbidden)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && !accountRoute && !bootstrapRoute && !strings.HasPrefix(r.URL.Path, "/api/diagnostics") && !(r.Method == "GET" && r.URL.Path == "/api/appearance") {
			epoch, unlocked := s.account.accessFor(s.hasTrustedView(r))
			if !unlocked {
				http.Error(w, "Cage 已锁定，请先登录", http.StatusLocked)
				return
			}
			ctx, cancel := context.WithCancel(r.Context())
			if strings.HasPrefix(r.URL.Path, "/api/mobile/") || strings.HasPrefix(r.URL.Path, "/api/cli/") {
				ctx = context.WithValue(ctx, mobileEpochKey{}, epoch)
			}
			stop := context.AfterFunc(epoch, cancel)
			defer func() { stop(); cancel() }()
			if phoneRoom != nil {
				stopPhone := context.AfterFunc(phoneRoom, cancel)
				defer stopPhone()
			}
			r = r.WithContext(ctx)
		}
		defer interruptHTTPOnCancel(w, r)()
		response := &diagnosticResponse{ResponseWriter: w}
		w = response
		defer func() {
			if response.status >= 400 && r.URL.Path != "/api/diagnostics/events" {
				_ = diagnostics.New(s.profile).Record("warn", "http", fmt.Sprintf("%s %s returned %d", r.Method, r.Pattern, response.status))
			}
		}()
		defer func() {
			if failure := recover(); failure != nil {
				diagnostics.Log("error", "http", fmt.Sprintf("handler panic: %v\n%s", failure, debug.Stack()))
				panic(failure)
			}
		}()
		if !storageRecoveryRoute(r) {
			if err := s.requireStorageReady(); err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func readJSON(w http.ResponseWriter, r *http.Request, v any, limits ...int64) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "需要 JSON 请求", 415)
		return false
	}
	limit := int64(64 << 10)
	if len(limits) > 0 {
		limit = limits[0]
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		http.Error(w, "请求格式无效", 400)
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		http.Error(w, "请求格式无效", 400)
		return false
	}
	return true
}
func jsonReply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
func requestError(w http.ResponseWriter, err error) {
	diagnostics.Log("error", "api", fmt.Sprintf("%v; cause=%v", err, errors.Unwrap(err)))
	http.Error(w, err.Error(), http.StatusBadRequest)
}
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if s.windowsGuest != nil && !s.storageRecoveryStatus().Required {
		s.refreshWindowsComponents()
	}
	projects, err := s.projectPaths()
	if err != nil {
		requestError(w, errors.New("无法读取项目列表，请检查项目配置"))
		return
	}
	running := s.vmRunning()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, e := os.Stat(filepath.Join(s.profile, "previous-profile.json"))
	projectError := ""
	if len(s.projectErrors) > 0 {
		projectError = "部分项目文件夹尚未连接，环境和浏览器可继续使用。"
	}
	if m := s.clientMounts[s.clientWorkspace]; s.clientWorkspace != "" && (m == nil || !m.live()) {
		projectError = "当前本机项目已断开，请重新共享或选择其他项目。"
	}
	view := map[string]any{
		"projects": projects, "ready": s.ready, "vm_running": running,
		"workspace": s.workspaceLocked(), "environment_workspace": s.workspaceLocked() == s.environmentDir(), "environment_directory": s.environmentDir(),
		"environment_identity_directory": cage.EnvironmentIdentityDir(s.profile),
		"local_vm_storage":               s.localVMStorage(), "project_error": projectError, "project_errors": s.projectErrors,
		"project_share_states": s.projectShareStatesLocked(),
		"storage_recovery":     s.storageRecoveryStatusLocked(),
		"keyboard":             s.preferences.Keyboard, "auto_timezone": s.preferences.AutoTimezone, "job": s.jobs.snapshot(),
		"location": s.location, "network_error": s.currentNetworkErrorLocked(), "can_restore": e == nil,
		"safety": s.safety, "environment_update_required": s.guestUpdateRequired, "environment_update_error": s.guestUpdateError,
	}
	if s.windowsGuest != nil {
		view["guest_system"] = guestos.Windows11
		view["guest_capabilities"] = s.windowsCapabilitiesLocked()
		view["location"] = nil
		if s.safety.Gate == nil && view["network_error"] == "" {
			view["network_error"] = "Windows 客机网络组件尚未连接，外网保持关闭"
		}
	} else if s.macosGuest != nil {
		view["guest_system"] = guestos.MacOS
	} else if system, err := s.installedLinuxSystem(); err != nil {
		view["guest_system_error"] = err.Error()
	} else if system != guestos.Debian13 {
		view["guest_system"] = system
	}
	jsonReply(w, view)
}
func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	c, err := loadConfig(s.configPath)
	if err != nil {
		requestError(w, err)
		return
	}
	s.mu.Lock()
	p := s.preferences
	s.mu.Unlock()
	jsonReply(w, settingsView(c, p))
}
func (s *Server) proposed(w http.ResponseWriter, r *http.Request) (savedProfile, bool) {
	var input Settings
	// Older clients omit preferences introduced after their release.
	s.mu.Lock()
	input.MobileRelay = s.preferences.MobileRelay
	input.BalancedDisplay = s.preferences.BalancedDisplay
	input.BrowserHomeURL = browserHomeURL(s.preferences)
	s.mu.Unlock()
	if !readJSON(w, r, &input) {
		return savedProfile{}, false
	}
	old, err := loadConfig(s.configPath)
	if err != nil {
		requestError(w, err)
		return savedProfile{}, false
	}
	c, p, err := proposedSettings(input, old, s.profile)
	if err != nil {
		requestError(w, err)
		return savedProfile{}, false
	}
	return savedProfile{c, p}, true
}
func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	v, ok := s.proposed(w, r)
	if !ok {
		return
	}
	if err := v.Config.RequireManagedProxy(s.profile); err != nil {
		requestError(w, err)
		return
	}
	loc, err := probeLocation(r.Context(), v.Config)
	if finishErr := s.finishLocationProbe(v.Config, loc, err); finishErr != nil {
		requestError(w, finishErr)
		return
	}
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, loc)
}
func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	v, ok := s.proposed(w, r)
	if !ok {
		return
	}
	kind := "settings"
	old, err := loadConfig(s.configPath)
	s.mu.Lock()
	previous := s.preferences
	s.mu.Unlock()
	if err == nil && old == v.Config && previous.AutoTimezone == v.Preferences.AutoTimezone && previous.AutoLanguage == v.Preferences.AutoLanguage && previous.Workspace != v.Preferences.Workspace {
		kind = "project"
	}
	s.launch(w, kind, func() error {
		s.mu.Lock()
		v.Preferences.BrowserHomeURL = s.preferences.BrowserHomeURL
		s.mu.Unlock()
		return s.apply(v, true)
	})
}
func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	var v savedProfile
	b, e := os.ReadFile(filepath.Join(s.profile, "previous-profile.json"))
	if e != nil || json.Unmarshal(b, &v) != nil {
		requestError(w, errors.New("没有可恢复的配置"))
		return
	}
	s.launch(w, "restore", func() error {
		if err := s.apply(v, false); err != nil {
			return err
		}
		return s.startEnvironment(false)
	})
}
func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	var input guestInstallRequest
	if r.ContentLength != 0 && !readJSON(w, r, &input) {
		return
	}
	if s.windowsGuest != nil {
		if input.System != "" && input.System != guestos.Windows11 {
			http.Error(w, "此配置已绑定 Windows 客机", http.StatusConflict)
			return
		}
		s.launch(w, "start", s.startWindowsGuest)
		return
	}
	if s.macosGuest != nil {
		if input.System != "" && input.System != guestos.MacOS {
			http.Error(w, "此配置已绑定 macOS 客机", http.StatusConflict)
			return
		}
		s.launch(w, "start", s.startMacOSGuest)
		return
	}
	if input.System == guestos.MacOS || input.System == guestos.Windows11 || input.CloneKey != "" {
		s.beginGuestInstall(w, r, input)
		return
	}
	pending, err := guestos.LoadInstallation(s.profile)
	if err != nil || (pending != nil && !hostimage.IsLinux(pending.System)) {
		http.Error(w, "已有原生客户机安装，请继续原安装", http.StatusConflict)
		return
	}
	if pending != nil && input.System != "" && pending.System != input.System {
		http.Error(w, "此环境已绑定所选 Linux 系统，请继续原安装", http.StatusConflict)
		return
	}
	if pending != nil {
		input.System = pending.System
		s.beginGuestInstall(w, r, input)
		return
	}
	boundSystem, err := s.installedLinuxSystem()
	if err != nil {
		requestError(w, err)
		return
	}
	if input.System != "" && input.System != boundSystem {
		if canInstallCatalogLinux(input.System) {
			s.beginGuestInstall(w, r, input)
		} else {
			http.Error(w, "当前环境无法启动所选客户机系统，请保留原环境并重新检查安装选择", http.StatusConflict)
		}
		return
	}
	s.launch(w, "start", func() error {
		if err := s.prepareEnvironment(); err != nil {
			return err
		}
		return s.startEnvironment(true)
	})
}
func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	s.requestStop(w, false)
}

func (s *Server) requestStop(w http.ResponseWriter, logout bool) {
	if err := s.requireStorageReady(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	for _, name := range []string{cage.InstanceName(s.profile, s.environmentDir()), "base-building"} {
		if owner := cage.InstanceJobID(filepath.Join(s.profile, "state"), name); owner != "" {
			_ = cage.RequestJobStop(s.profile, owner)
		}
	}
	jobID := s.jobs.cancelRunning()
	if jobID != "" {
		_ = cage.RequestJobStop(s.profile, jobID)
	}
	if s.macosGuest != nil {
		// Stopping the guest revokes its current grant. A user's saved pause
		// remains distinct so the next Mac start can honor the same intent.
		_ = s.revokeNetworkNow("正在停止环境", false)
	} else if s.windowsGuest == nil {
		_ = s.pauseNetworkNow("正在停止环境")
	}
	kind := "stop"
	if logout {
		kind = "logout"
	}
	s.launch(w, kind, s.stopEnvironment)
}

func (s *Server) cookieName() string { u, _ := url.Parse(s.origin); return cookieName + "_" + u.Port() }

func (s *Server) issueTicket() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tickets == nil {
		s.tickets = map[string]time.Time{}
	}
	for token, expires := range s.tickets {
		if time.Now().After(expires) {
			delete(s.tickets, token)
		}
	}
	token := randomToken()
	s.tickets[token] = time.Now().Add(time.Minute)
	return token
}
func (s *Server) consumeTicket(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.tickets[token]
	delete(s.tickets, token)
	return ok && time.Now().Before(expires)
}
