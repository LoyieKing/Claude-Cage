package workbench

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sylvinus/agent-vm/internal/guestos"
	"golang.org/x/crypto/ssh"
)

func newTestSigner(t *testing.T) (ed25519.PrivateKey, ssh.Signer) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return private, signer
}

func writeTestPrivateKey(t *testing.T, path string, private ed25519.PrivateKey) {
	t.Helper()
	block, err := ssh.MarshalPrivateKey(private, "cage-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
}

func servePinnedSSH(t *testing.T, host ssh.Signer, client ssh.PublicKey) (int, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), client.Marshal()) {
			return nil, os.ErrPermission
		}
		return &ssh.Permissions{}, nil
	}}
	config.AddHostKey(host)
	var accepts atomic.Int32
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			go func() {
				defer connection.Close()
				server, channels, requests, err := ssh.NewServerConn(connection, config)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for next := range channels {
					_ = next.Reject(ssh.Prohibited, "closed")
				}
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port, &accepts
}

func writeGuestSSHConfig(t *testing.T, s *Server, port int, identity, known, alias string, lima bool) {
	t.Helper()
	dir := s.instanceDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var body string
	if lima {
		body = "Host " + s.sshTarget() + "\n" +
			"  IdentityFile " + strconv.Quote(identity) + "\n" +
			"  User cage\n" +
			"  Hostname 127.0.0.1\n" +
			"  Port " + strconv.Itoa(port) + "\n" +
			"  StrictHostKeyChecking no\n" +
			"  UserKnownHostsFile /dev/null\n" +
			"  NoHostAuthenticationForLocalhost yes\n" +
			"  IdentitiesOnly yes\n"
	} else {
		body = "Host " + s.sshTarget() + "\n" +
			"  HostName 127.0.0.1\n" +
			"  Port " + strconv.Itoa(port) + "\n" +
			"  User cage\n" +
			"  IdentityFile " + strconv.Quote(identity) + "\n" +
			"  IdentitiesOnly yes\n" +
			"  HostKeyAlias " + alias + "\n" +
			"  HostKeyAlgorithms ssh-ed25519\n" +
			"  UserKnownHostsFile " + strconv.Quote(known) + "\n" +
			"  StrictHostKeyChecking yes\n" +
			"  PasswordAuthentication no\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh.config"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDialGuestSSHPinsContainerAndRejectsReplacement(t *testing.T) {
	s := testServer(t)
	if err := os.WriteFile(filepath.Join(s.profile, "container.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	clientPrivate, clientSigner := newTestSigner(t)
	_, hostSigner := newTestSigner(t)
	port, _ := servePinnedSSH(t, hostSigner, clientSigner.PublicKey())
	identity := filepath.Join(s.instanceDir(), "id_ed25519")
	known := filepath.Join(s.instanceDir(), "known_hosts")
	writeTestPrivateKey(t, identity, clientPrivate)
	alias := "cage-container-test"
	line := alias + " " + string(ssh.MarshalAuthorizedKey(hostSigner.PublicKey()))
	if err := os.WriteFile(known, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	writeGuestSSHConfig(t, s, port, identity, known, alias, false)
	calls := 0
	previous := readLimaHostKey
	readLimaHostKey = func(*Server, context.Context) ([]byte, error) {
		calls++
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { readLimaHostKey = previous })
	client, err := s.dialGuestSSH(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if calls != 0 {
		t.Fatal("container dial used the Lima bootstrap")
	}
	_, other := newTestSigner(t)
	port, _ = servePinnedSSH(t, other, clientSigner.PublicKey())
	before, err := os.ReadFile(known)
	if err != nil {
		t.Fatal(err)
	}
	writeGuestSSHConfig(t, s, port, identity, known, alias, false)
	if _, err = s.dialGuestSSH(context.Background()); err == nil || !strings.Contains(err.Error(), "主机身份不匹配") {
		t.Fatal(err)
	}
	after, err := os.ReadFile(known)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("replacement rewrote the pinned host key", err)
	}
}

func TestDialGuestSSHLimaProvisionsOnceThenRejectsChange(t *testing.T) {
	s := testServer(t)
	clientPrivate, clientSigner := newTestSigner(t)
	_, hostSigner := newTestSigner(t)
	port, accepts := servePinnedSSH(t, hostSigner, clientSigner.PublicKey())
	identity := filepath.Join(s.instanceDir(), "id_ed25519")
	writeTestPrivateKey(t, identity, clientPrivate)
	writeGuestSSHConfig(t, s, port, identity, "", "", true)
	var calls atomic.Int32
	previous := readLimaHostKey
	readLimaHostKey = func(*Server, context.Context) ([]byte, error) {
		calls.Add(1)
		return ssh.MarshalAuthorizedKey(hostSigner.PublicKey()), nil
	}
	t.Cleanup(func() { readLimaHostKey = previous })
	client, err := s.dialGuestSSH(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	known := filepath.Join(s.instanceDir(), "known_hosts")
	pinned, err := os.ReadFile(known)
	if err != nil || !bytes.Contains(pinned, bytes.TrimSpace(ssh.MarshalAuthorizedKey(hostSigner.PublicKey()))) {
		t.Fatal("bootstrap did not pin the host key", err)
	}
	second, err := s.dialGuestSSH(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = second.Close()
	if calls.Load() != 1 {
		t.Fatalf("bootstrap ran %d times", calls.Load())
	}
	_, other := newTestSigner(t)
	otherPort, otherAccepts := servePinnedSSH(t, other, clientSigner.PublicKey())
	writeGuestSSHConfig(t, s, otherPort, identity, "", "", true)
	if _, err = s.dialGuestSSH(context.Background()); err == nil || !strings.Contains(err.Error(), "主机身份不匹配") {
		t.Fatal(err)
	}
	after, err := os.ReadFile(known)
	if err != nil || !bytes.Equal(pinned, after) {
		t.Fatal("changed host key was written back", err)
	}
	if otherAccepts.Load() == 0 && accepts.Load() == 0 {
		t.Fatal("pinned dial never reached the guest")
	}
}

func TestDialGuestSSHRejectsGlobalFallbackBeforeBootstrap(t *testing.T) {
	s := testServer(t)
	dir := s.instanceDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(dir, "id_ed25519")
	private, signer := newTestSigner(t)
	writeTestPrivateKey(t, identity, private)
	_, host := newTestSigner(t)
	port, accepts := servePinnedSSH(t, host, signer.PublicKey())
	marker := filepath.Join(t.TempDir(), "proxy-ran")
	base := "Host " + s.sshTarget() + "\n  Hostname 127.0.0.1\n  Port " + strconv.Itoa(port) + "\n  User cage\n  IdentityFile " + strconv.Quote(identity) + "\n  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n"
	bodies := []string{
		"Include /tmp/cage-user-ssh-config\n" + base,
		base + "\nHost other\n  Match all\n  ProxyCommand touch " + marker + "\n  ForwardAgent yes\n",
	}
	var calls atomic.Int32
	previous := readLimaHostKey
	readLimaHostKey = func(*Server, context.Context) ([]byte, error) {
		calls.Add(1)
		return ssh.MarshalAuthorizedKey(host.PublicKey()), nil
	}
	t.Cleanup(func() { readLimaHostKey = previous })
	for _, body := range bodies {
		if err := os.WriteFile(filepath.Join(dir, "ssh.config"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.dialGuestSSH(context.Background()); err == nil {
			t.Fatal("weakened config was dialed")
		}
	}
	if calls.Load() != 0 || accepts.Load() != 0 {
		t.Fatalf("fallback still ran bootstrap=%d accepts=%d", calls.Load(), accepts.Load())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("proxy command was executed", err)
	}
}

func TestDialGuestSSHRejectsNonLinuxGuest(t *testing.T) {
	s := testServer(t)
	s.macosGuest = &guestos.MacOSProfile{}
	if _, err := s.dialGuestSSH(context.Background()); err == nil {
		t.Fatal("macOS guest returned an SSH client")
	}
}
