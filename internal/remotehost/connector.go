package remotehost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/host"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

// ServerEntryCommand accepts two path fields, never an arbitrary shell command.
func ServerEntryCommand(p HostProfile) (string, error) {
	p, err := normalizeProfile(p)
	if err != nil {
		return "", err
	}
	command := shellQuote(p.Executable) + " server-entry --json"
	if p.ConfigPath != "" {
		command = "env CLAUDE_CAGE_CONFIG=" + shellQuote(p.ConfigPath) + " " + command
	}
	return "exec " + command, nil
}

// ValidateManifest limits every forwarded connection to a declared loopback
// service. Instance identity persists independently of network addresses.
func ValidateManifest(m ServerManifest) error {
	if m.APIProtocol != APIProtocol {
		return failure("protocol_incompatible", "云端 Cage 协议与客户端不兼容，请更新到兼容版本")
	}
	if !instancePattern.MatchString(m.InstanceID) || m.Platform != "linux" || (m.Runtime != "container" && m.Runtime != "lima") || len(m.Version) > 128 || hasControl(m.Version) {
		return failure("manifest_invalid", "云端 Cage 身份响应无效")
	}
	u, err := url.Parse(m.Origin)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") {
		return failure("origin_refused", "云端服务必须使用明确的本机回环地址")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return failure("origin_refused", "云端服务端口无效")
	}
	if len(m.Ticket) < 32 || len(m.Ticket) > 512 || hasControl(m.Ticket) || len(m.Capabilities) > 64 {
		return failure("manifest_invalid", "云端 Cage 连接票据无效")
	}
	for key := range m.Capabilities {
		if key == "" || len(key) > 64 || hasControl(key) {
			return failure("manifest_invalid", "云端能力声明无效")
		}
	}
	return nil
}

func authentication(ctx context.Context, p HostProfile, secrets ConnectSecrets) (ssh.AuthMethod, func(), error) {
	if p.Auth.Kind == "agent" {
		conn, err := dialAgent(ctx)
		if err != nil {
			return nil, nil, failure("agent_unavailable", "无法连接本机 SSH agent，请启动 agent 或选择密钥文件")
		}
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		signers, err := agent.NewClient(conn).Signers()
		if err != nil || len(signers) == 0 {
			stop()
			conn.Close()
			return nil, nil, failure("agent_unavailable", "SSH agent 中没有可用密钥")
		}
		return ssh.PublicKeys(signers...), func() { stop(); conn.Close() }, nil
	}
	if err := host.CheckPrivateFile(p.Auth.IdentityFile); err != nil {
		return nil, nil, failure("identity_permissions", "SSH 密钥不存在或文件权限不安全")
	}
	f, err := os.Open(p.Auth.IdentityFile)
	if err != nil {
		return nil, nil, failure("identity_read", "无法读取 SSH 密钥文件")
	}
	key, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	f.Close()
	defer clear(key)
	if err != nil || len(key) > 1<<20 {
		return nil, nil, failure("identity_read", "SSH 密钥文件无效或过大")
	}
	var signer ssh.Signer
	if len(secrets.Passphrase) > 0 {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(key, secrets.Passphrase)
	} else {
		signer, err = ssh.ParsePrivateKey(key)
	}
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, nil, failure("passphrase_required", "此 SSH 密钥需要口令")
		}
		return nil, nil, failure("identity_invalid", "SSH 密钥或口令无效")
	}
	return ssh.PublicKeys(signer), func() {}, nil
}

type limitedOutput struct {
	bytes.Buffer
	limit int
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	if w.Len()+len(p) > w.limit {
		return 0, errors.New("SSH response exceeds limit")
	}
	return w.Buffer.Write(p)
}

func connect(lifetime context.Context, p HostProfile, secrets ConnectSecrets, phase func(string)) (*Session, error) {
	p, err := normalizeProfile(p)
	if err != nil {
		return nil, err
	}
	if secrets.AttachOnly && onAttachDial != nil {
		onAttachDial(lifetime)
		if lifetime.Err() != nil {
			return nil, failure("connection_cancelled", "远程连接已取消")
		}
	}
	bootstrap, cancelBootstrap := context.WithTimeout(lifetime, 45*time.Second)
	defer cancelBootstrap()
	phase("ssh_auth")
	auth, cleanup, err := authentication(bootstrap, p, secrets)
	clear(secrets.Passphrase)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	phase("ssh_connect")
	address := net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
	raw, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(bootstrap, "tcp", address)
	if err != nil {
		return nil, failure("ssh_connect_failed", "无法连接 SSH 主机，请检查地址、端口和网络")
	}
	complete := false
	defer func() {
		if !complete {
			raw.Close()
		}
	}()
	_ = raw.SetDeadline(time.Now().Add(20 * time.Second))
	stopBootstrap := context.AfterFunc(bootstrap, func() { raw.Close() })
	defer stopBootstrap()
	var keyError *Error
	config := &ssh.ClientConfig{User: p.User, Auth: []ssh.AuthMethod{auth}, HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		observed := &HostKey{Algorithm: key.Type(), Fingerprint: ssh.FingerprintSHA256(key)}
		if p.HostKey == nil {
			keyError = &Error{Code: "host_key_unknown", Message: "请核对并确认 SSH 主机密钥指纹后重新连接", HostKey: observed}
			return keyError
		}
		if p.HostKey.Algorithm != observed.Algorithm || p.HostKey.Fingerprint != observed.Fingerprint {
			keyError = &Error{Code: "host_key_changed", Message: "SSH 主机密钥已变化，请核对主机身份后更新指纹", HostKey: observed}
			return keyError
		}
		return nil
	}}
	if p.HostKey != nil {
		config.HostKeyAlgorithms = []string{p.HostKey.Algorithm}
		if p.HostKey.Algorithm == ssh.KeyAlgoRSA {
			// The public-key type is ssh-rsa, while modern RSA signatures use
			// the SHA-2 negotiation names. Do not enable obsolete SHA-1 signatures.
			config.HostKeyAlgorithms = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
		}
	}
	sshConn, channels, requests, err := ssh.NewClientConn(raw, address, config)
	if err != nil {
		if keyError != nil {
			return nil, keyError
		}
		return nil, failure("ssh_auth_failed", "SSH 认证未完成，请检查用户名和密钥")
	}
	_ = raw.SetDeadline(time.Time{})
	client := ssh.NewClient(sshConn, channels, requests)
	defer func() {
		if !complete {
			client.Close()
		}
	}()
	phase("server_handshake")
	if !stopBootstrap() || bootstrap.Err() != nil {
		return nil, failure("ssh_timeout", "SSH 认证超时")
	}
	timeout := 2 * time.Minute
	if secrets.Deploy {
		timeout = 90 * time.Minute
	}
	ready, cancelReady := context.WithTimeout(lifetime, timeout)
	defer cancelReady()
	stopReady := context.AfterFunc(ready, func() { client.Close() })
	defer stopReady()
	output, err := cloudEntry(ready, client, p, secrets.Deploy && !secrets.AttachOnly, secrets.AttachOnly, phase)
	if err != nil {
		return nil, err
	}
	var manifest ServerManifest
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&manifest) != nil {
		return nil, failure("manifest_invalid", "云端 Cage 未返回有效连接信息")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, failure("manifest_invalid", "云端 Cage 返回了多余连接信息")
	}
	if err = ValidateManifest(manifest); err != nil {
		return nil, err
	}
	if secrets.ExpectInstanceID != "" && manifest.InstanceID != secrets.ExpectInstanceID {
		return nil, failure("instance_changed", "云端 Cage 实例已变化，已停止自动重连。请核对主机身份后重新连接。")
	}
	origin, _ := url.Parse(manifest.Origin)
	targetHost := origin.Hostname()
	if targetHost == "localhost" {
		targetHost = "127.0.0.1"
	}
	target := net.JoinHostPort(targetHost, origin.Port())
	life, cancelLife := context.WithCancel(lifetime)
	transport := &http.Transport{Proxy: nil, ForceAttemptHTTP2: false, MaxIdleConns: 32, MaxIdleConnsPerHost: 16, IdleConnTimeout: 60 * time.Second, ResponseHeaderTimeout: 30 * time.Second, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != origin.Host {
			return nil, failure("origin_refused", "拒绝访问云端声明之外的服务")
		}
		return client.DialContext(ctx, "tcp", target)
	}}
	jar, _ := cookiejar.New(nil)
	bound := &sessionTransport{base: transport, jar: jar, origin: manifest.Origin, ctx: life}
	var once sync.Once
	s := &Session{Manifest: manifest.Public(), Transport: bound, Context: life, Origin: manifest.Origin}
	shutdown := func() {
		once.Do(func() {
			cancelLife()
			client.Close()
			transport.CloseIdleConnections()
			bound.closeStreams()
		})
	}
	s.close = func() error {
		shutdown()
		return nil
	}
	bound.invalidate = func() error {
		s.mark("remote_session_expired")
		return s.Close()
	}
	s.Client = &http.Client{Transport: bound, CheckRedirect: func(*http.Request, []*http.Request) error {
		return failure("redirect_refused", "云端服务重定向已拒绝")
	}}
	body, _ := json.Marshal(map[string]string{"token": manifest.Ticket})
	manifest.Ticket = ""
	req, _ := http.NewRequestWithContext(ready, "POST", s.Origin+"/api/session", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	response, err := s.Client.Do(req)
	clear(body)
	if err != nil {
		s.Close()
		return nil, failure("server_session_failed", "无法建立云端 Cage 会话")
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(jar.Cookies(origin)) == 0 {
		s.Close()
		return nil, failure("server_session_failed", "云端 Cage 未接受连接票据")
	}
	if ready.Err() != nil {
		s.Close()
		return nil, failure("connection_cancelled", "远程连接已取消")
	}
	stopReady()
	complete = true
	interval, timeout := heartbeatInterval, heartbeatTimeout
	var helpers sync.WaitGroup
	helpers.Add(2)
	s.close = func() error {
		shutdown()
		helpers.Wait()
		return nil
	}
	go func() {
		defer helpers.Done()
		_ = client.Wait()
		shutdown()
	}()
	go func() {
		defer helpers.Done()
		watchHeartbeat(life, client, interval, timeout, shutdown)
	}()
	context.AfterFunc(life, func() { _ = s.Close() })
	return s, nil
}

type sessionTransport struct {
	base          *http.Transport
	jar           http.CookieJar
	origin        string
	ctx           context.Context
	invalidate    func() error
	mu            sync.Mutex
	streams       []io.Closer
	streamsClosed bool
	armOnce       sync.Once
}

func (t *sessionTransport) arm() {
	t.armOnce.Do(func() {
		if t.ctx == nil {
			return
		}
		context.AfterFunc(t.ctx, func() { t.closeStreams() })
	})
}

func (t *sessionTransport) track(body io.Closer) {
	t.mu.Lock()
	closed := t.streamsClosed
	if !closed {
		t.streams = append(t.streams, body)
	}
	t.mu.Unlock()
	if closed {
		body.Close()
	}
}

func (t *sessionTransport) untrack(body io.Closer) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, item := range t.streams {
		if item == body {
			t.streams = append(t.streams[:i], t.streams[i+1:]...)
			return
		}
	}
}

func (t *sessionTransport) closeStreams() {
	t.mu.Lock()
	t.streamsClosed = true
	all := t.streams
	t.streams = nil
	t.mu.Unlock()
	for _, body := range all {
		body.Close()
	}
}

func watchHeartbeat(ctx context.Context, client *ssh.Client, interval, timeout time.Duration, lost func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := pingSSH(client, timeout); err != nil {
				lost()
				return
			}
		}
	}
}

func pingSSH(client *ssh.Client, timeout time.Duration) error {
	finished := make(chan error, 1)
	go func() {
		_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		finished <- err
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-finished:
		return err
	case <-timer.C:
		_ = client.Close()
		<-finished
		return errors.New("ssh heartbeat timeout")
	}
}

func (t *sessionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.arm()
	if request.URL.Scheme+"://"+request.URL.Host != t.origin || request.URL.User != nil {
		return nil, failure("origin_refused", "拒绝访问云端声明之外的服务")
	}
	if err := t.ctx.Err(); err != nil {
		return nil, failure("connection_changed", "远程连接已断开")
	}
	ctx, cancel := context.WithCancel(request.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	finish := func() { stop(); cancel() }
	req := request.Clone(ctx)
	req.Header = request.Header.Clone()
	req.Host = req.URL.Host
	req.Header.Del("Cookie")
	req.Header.Del("Authorization")
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Idempotency-Key")
	req.Header.Del("X-Idempotency-Key")
	req.GetBody = nil
	req.Header.Set("Origin", t.origin)
	for _, cookie := range t.jar.Cookies(req.URL) {
		req.AddCookie(cookie)
	}
	response, err := t.base.RoundTrip(req)
	if err != nil {
		finish()
		return nil, failure("remote_request_failed", "远程请求中断，请检查连接；已提交的操作不会自动重发")
	}
	if response.StatusCode == http.StatusUnauthorized {
		response.Body.Close()
		finish()
		if t.invalidate != nil {
			_ = t.invalidate()
		}
		return nil, failure("remote_session_expired", "云端会话已失效，请重新连接并核对云端任务")
	}
	t.jar.SetCookies(req.URL, response.Cookies())
	response.Header.Del("Set-Cookie")
	if response.StatusCode >= 300 && response.StatusCode < 400 && response.Header.Get("Location") != "" {
		response.Body.Close()
		finish()
		return nil, failure("redirect_refused", "云端服务重定向已拒绝")
	}
	if rw, ok := response.Body.(io.ReadWriteCloser); ok {
		wrapped := &cancelReadWriteCloser{ReadWriteCloser: rw, finish: finish}
		wrapped.release = func() { t.untrack(wrapped) }
		response.Body = wrapped
		t.track(wrapped)
	} else {
		wrapped := &cancelReadCloser{ReadCloser: response.Body, finish: finish}
		wrapped.release = func() { t.untrack(wrapped) }
		response.Body = wrapped
		t.track(wrapped)
	}
	return response, nil
}

type cancelReadCloser struct {
	io.ReadCloser
	finish      func()
	release     func()
	once        sync.Once
	releaseOnce sync.Once
}

func (b *cancelReadCloser) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	if e != nil {
		b.once.Do(b.finish)
		b.retire()
	}
	return n, e
}
func (b *cancelReadCloser) retire() {
	b.releaseOnce.Do(func() {
		if b.release != nil {
			b.release()
		}
	})
}
func (b *cancelReadCloser) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.finish)
	b.retire()
	return err
}

type cancelReadWriteCloser struct {
	io.ReadWriteCloser
	finish      func()
	release     func()
	once        sync.Once
	releaseOnce sync.Once
}

func (b *cancelReadWriteCloser) Read(p []byte) (int, error) {
	n, e := b.ReadWriteCloser.Read(p)
	if e != nil {
		b.once.Do(b.finish)
		b.retire()
	}
	return n, e
}
func (b *cancelReadWriteCloser) retire() {
	b.releaseOnce.Do(func() {
		if b.release != nil {
			b.release()
		}
	})
}
func (b *cancelReadWriteCloser) Close() error {
	err := b.ReadWriteCloser.Close()
	b.once.Do(b.finish)
	b.retire()
	return err
}
