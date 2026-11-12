package workbench

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/remotehost"
	"golang.org/x/crypto/ssh"
)

func TestDesktopReconnectRejectsPreviousGeneration(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/session" {
			var body struct{ Token string }
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Token != strings.Repeat("t", 64) {
				http.Error(w, "ticket", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "remote_session", Value: "private-cookie", Path: "/", HttpOnly: true})
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ok":true}`)
			return
		}
		if cookie, err := r.Cookie("remote_session"); err != nil || cookie.Value != "private-cookie" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer httpServer.Close()
	_, hostPrivate, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPrivate)
	_, clientPrivate, _ := ed25519.GenerateKey(rand.Reader)
	clientSigner, _ := ssh.NewSignerFromKey(clientPrivate)
	keyPath := filepath.Join(t.TempDir(), "identity")
	keyBytes, _ := x509.MarshalPKCS8PrivateKey(clientPrivate)
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	instanceID := strings.Repeat("a", 64)
	manifest := remotehost.ServerManifest{APIProtocol: 1, InstanceID: instanceID, Version: "test", Platform: "linux", Runtime: "container", Origin: httpServer.URL, Ticket: strings.Repeat("t", 64), Capabilities: map[string]bool{"terminal": true}}
	var conns sync.Map
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if string(key.Marshal()) != string(clientSigner.PublicKey().Marshal()) {
			return nil, os.ErrPermission
		}
		return nil, nil
	}}
	config.AddHostKey(hostSigner)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conns.Store(conn, true)
			go serveDesktopSSH(conn, &conns, config, manifest)
		}
	}()
	s, _ := gatewayFixture(t)
	store := s.remote.store
	profile := remotehost.HostProfile{Name: "desktop", Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, User: "cage", Auth: remotehost.AuthConfig{Kind: "key_file", IdentityFile: keyPath}, HostKey: &remotehost.HostKey{Algorithm: hostSigner.PublicKey().Type(), Fingerprint: ssh.FingerprintSHA256(hostSigner.PublicKey())}}
	profile, err = store.Upsert(profile)
	if err != nil {
		t.Fatal(err)
	}
	manager := remotehost.NewManager(store)
	if _, err = manager.Connect(profile.ID, remotehost.ConnectSecrets{}); err != nil {
		t.Fatal(err)
	}
	first := waitDesktopConnected(t, manager)
	d := s.remote.desktop
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	target := &desktopTarget{id: first.ConnectionID, mode: "remote", remote: manager, ctx: ctx, cancel: cancel}
	d.mu.Lock()
	d.generation, d.active = first.ConnectionID, target
	d.mu.Unlock()

	conns.Range(func(key, _ any) bool { key.(net.Conn).Close(); return true })
	next := waitDesktopRotated(t, manager, first.ConnectionID)
	if next.InstanceID != instanceID || next.HostID != profile.ID {
		t.Fatalf("transport changed instance: %+v", next)
	}
	if _, _, err = d.acquire(first.ConnectionID); err == nil {
		t.Fatal("old desktop generation was accepted before a context poll")
	}
	view := d.context()
	if view.ConnectionID != next.ConnectionID || view.InstanceID != instanceID || view.Mode != "remote" {
		t.Fatalf("desktop generation did not follow the new transport: %+v", view)
	}
	if _, _, err = d.acquire(view.ConnectionID); err != nil {
		t.Fatal(err)
	}
	if got := gatewayRequest(s, "/api/status", first.ConnectionID); got.Code != http.StatusConflict {
		t.Fatalf("old desktop id status = %d", got.Code)
	}
	if got := gatewayRequest(s, "/api/status", view.ConnectionID); got.Code != http.StatusOK {
		t.Fatalf("new desktop id status = %d %s", got.Code, got.Body.String())
	}
}

func waitDesktopConnected(t *testing.T, m *remotehost.Manager) remotehost.ConnectionState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state := m.Snapshot()
		if state.State == "connected" {
			return state
		}
		if state.State == "failed" || state.State == "needs_auth" {
			t.Fatalf("desktop ssh failed: %+v", state)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("desktop ssh did not connect: %+v", m.Snapshot())
	return remotehost.ConnectionState{}
}

func waitDesktopRotated(t *testing.T, m *remotehost.Manager, old string) remotehost.ConnectionState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state := m.Snapshot()
		if state.State == "connected" && state.ConnectionID != "" && state.ConnectionID != old {
			return state
		}
		if state.State == "failed" || state.State == "needs_auth" {
			t.Fatalf("reconnect stopped: %+v", state)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("desktop transport did not rotate: %+v", m.Snapshot())
	return remotehost.ConnectionState{}
}

func serveDesktopSSH(conn net.Conn, conns *sync.Map, config *ssh.ServerConfig, manifest remotehost.ServerManifest) {
	defer conns.Delete(conn)
	defer conn.Close()
	server, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer server.Close()
	go ssh.DiscardRequests(requests)
	for channel := range channels {
		go func(in ssh.NewChannel) {
			switch in.ChannelType() {
			case "session":
				channel, requests, err := in.Accept()
				if err != nil {
					return
				}
				defer channel.Close()
				for request := range requests {
					if request.Type != "exec" {
						request.Reply(false, nil)
						continue
					}
					request.Reply(true, nil)
					json.NewEncoder(channel).Encode(manifest)
					channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
					return
				}
			case "direct-tcpip":
				var forward struct {
					Host       string
					Port       uint32
					Origin     string
					OriginPort uint32
				}
				if ssh.Unmarshal(in.ExtraData(), &forward) != nil {
					in.Reject(ssh.Prohibited, "invalid")
					return
				}
				_, uPort, _ := splitOrigin(manifest.Origin)
				if forward.Host != "127.0.0.1" || int(forward.Port) != uPort {
					in.Reject(ssh.Prohibited, "wrong destination")
					return
				}
				peer, err := net.Dial("tcp", net.JoinHostPort(forward.Host, strconv.Itoa(int(forward.Port))))
				if err != nil {
					in.Reject(ssh.ConnectionFailed, "dial")
					return
				}
				defer peer.Close()
				channel, requests, err := in.Accept()
				if err != nil {
					return
				}
				defer channel.Close()
				go ssh.DiscardRequests(requests)
				done := make(chan struct{})
				go func() { io.Copy(peer, channel); peer.Close(); close(done) }()
				io.Copy(channel, peer)
				channel.Close()
				<-done
			default:
				in.Reject(ssh.UnknownChannelType, "unsupported")
			}
		}(channel)
	}
}

func splitOrigin(origin string) (string, int, bool) {
	u := strings.TrimPrefix(origin, "http://")
	host, portText, ok := strings.Cut(u, ":")
	port, err := strconv.Atoi(portText)
	if !ok || err != nil {
		return "", 0, false
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	return host, port, true
}
