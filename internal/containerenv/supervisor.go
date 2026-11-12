package containerenv

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/lima-vm/sshocker/pkg/reversesshfs"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/host"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

var (
	errShareBlocked   = errors.New("容器项目共享挂载身份不匹配，拒绝卸载")
	errGuestIdentity  = errors.New("容器 SSH 主机身份不匹配")
	errShareUnmounted = errors.New("容器项目共享已断开")
)

// shareIdentity is the guest mount Cage itself created. Cleanup may unmount
// only when the live mount still has this root identity. Path equality alone
// is not authorization, and the underlying directory stays root-owned and
// unwritable so a disappeared mount cannot capture new writes.
type shareIdentity struct {
	Mountpoint       string `json:"mountpoint"`
	Source           string `json:"source"`
	FSType           string `json:"fstype"`
	MountDevice      string `json:"mount_device"`
	UnderlyingDevice string `json:"underlying_device,omitempty"`
	// HostDev and HostIno are copies of the separate host-root pin for the
	// attempt that proved the guest mount. They are not a new authorization.
	HostDev uint64 `json:"host_dev,omitempty"`
	HostIno uint64 `json:"host_ino,omitempty"`
}

// hostRootRecord is the default workspace directory actually opened on the
// host. It is stored apart from the guest mount proof and is never rewritten
// to follow a replaced EnvironmentDir.
type hostRootRecord struct {
	Path string `json:"path"`
	Dev  uint64 `json:"dev"`
	Ino  uint64 `json:"ino"`
}

type mountpointFacts struct {
	Exists   bool
	Symlink  bool
	NonEmpty bool
	UID      int
	Mode     string
	Device   string
}

type guestDialer struct {
	mu     sync.Mutex
	client *ssh.Client
}

func (d *guestDialer) Dial(network, address string) (net.Conn, error) {
	d.mu.Lock()
	client := d.client
	d.mu.Unlock()
	if client == nil {
		return nil, errors.New("容器 SSH 尚未连接")
	}
	return client.Dial(network, address)
}

func (d *guestDialer) Current() *ssh.Client {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.client
}

func (d *guestDialer) Set(client *ssh.Client) {
	d.mu.Lock()
	old := d.client
	d.client = client
	d.mu.Unlock()
	if old != nil && old != client {
		_ = old.Close()
	}
}

func (d *guestDialer) Clear(client *ssh.Client) {
	d.mu.Lock()
	if d.client == client {
		d.client = nil
	}
	d.mu.Unlock()
}

func (d *guestDialer) Close() { d.Set(nil) }

type recordPublisher struct {
	mu     sync.Mutex
	path   string
	record networkRecord
}

func newRecordPublisher(path string, record networkRecord) *recordPublisher {
	return &recordPublisher{path: path, record: record}
}

func (p *recordPublisher) write() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return writePrivateJSON(p.path, p.record)
}

func (p *recordPublisher) setShare(status string, ready bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record.ShareStatus = status
	p.record.Shared = ready
	return writePrivateJSON(p.path, p.record)
}

func (p *recordPublisher) owner() networkRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.record
}

func decideSharePrepare(mounted bool, current shareIdentity, recorded *shareIdentity) error {
	if !mounted {
		return nil
	}
	if recorded != nil && shareCleanupAllowed(current, *recorded) {
		return nil
	}
	return errShareBlocked
}

func shareCleanupAllowed(current, recorded shareIdentity) bool {
	return current.Mountpoint != "" && current.Mountpoint == recorded.Mountpoint &&
		current.FSType == "fuse.sshfs" && recorded.FSType == "fuse.sshfs" &&
		current.Source != "" && current.Source == recorded.Source &&
		current.MountDevice != "" && current.MountDevice == recorded.MountDevice
}

func decideProtect(facts mountpointFacts, recorded *shareIdentity) error {
	if facts.Symlink {
		return errShareBlocked
	}
	if recorded != nil && recorded.UnderlyingDevice != "" && facts.Exists && facts.Device != "" && facts.Device != recorded.UnderlyingDevice {
		return errShareBlocked
	}
	if facts.Exists && facts.NonEmpty && (recorded == nil || recorded.UnderlyingDevice == "") {
		return errShareBlocked
	}
	return nil
}

func shareRetry(err error) (blocked, retry bool) {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, errShareUnmounted) {
		return false, true
	}
	if errors.Is(err, errShareBlocked) || errors.Is(err, errGuestIdentity) {
		return true, false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, false
	}
	return false, true
}

func nextBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next > 5*time.Second || next < current {
		return 5 * time.Second
	}
	return next
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// retryUntilCancelled reconnects one fault domain until ctx ends. A blocked
// identity is not retried destructively and does not become a network-worker
// failure; the caller keeps the TAP and host SSH listener up.
func retryUntilCancelled(ctx context.Context, run func(context.Context) error, onStatus func(error)) error {
	backoff := 200 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := run(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if onStatus != nil {
			onStatus(err)
		}
		blocked, retry := shareRetry(err)
		if blocked {
			<-ctx.Done()
			return ctx.Err()
		}
		if !retry {
			return err
		}
		if !sleepContext(ctx, backoff) {
			return ctx.Err()
		}
		if err == nil || errors.Is(err, io.EOF) || errors.Is(err, errShareUnmounted) {
			backoff = 200 * time.Millisecond
			continue
		}
		backoff = nextBackoff(backoff)
	}
}

func waitNetworkExit(ctx context.Context, networkDone, sshDone <-chan error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-networkDone:
		if err == nil || errors.Is(err, context.Canceled) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("容器网络连接已断开")
		}
		return err
	case err := <-sshDone:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			return errors.New("容器 SSH 入口已断开")
		}
		return err
	}
}

func (r *Runtime) watchRuntimeIntent(ctx context.Context, generation uint64, cancel context.CancelFunc) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			intent, err := r.readIntent()
			if err != nil || intent.Desired != intentRunning || intent.Generation != generation {
				cancel()
				return
			}
		}
	}
}

func (r *Runtime) superviseGuestServices(ctx context.Context, network *Network, publisher *recordPublisher) {
	recorded, err := r.loadShareIdentity()
	if err != nil {
		_ = publisher.setShare(shareStatusBlocked, false)
		<-ctx.Done()
		return
	}
	var current *shareIdentity
	if recorded != nil {
		current = recorded
	}
	_ = retryUntilCancelled(ctx, func(ctx context.Context) error {
		id, err := r.runShareOnce(ctx, network, current, func(shareIdentity) {
			_ = publisher.setShare(shareStatusReady, true)
		})
		if id != nil {
			current = id
		}
		return err
	}, func(err error) {
		switch {
		case errors.Is(err, errShareBlocked) || errors.Is(err, errGuestIdentity):
			_ = publisher.setShare(shareStatusBlocked, false)
		case err == nil || errors.Is(err, io.EOF) || errors.Is(err, errShareUnmounted):
			_ = publisher.setShare(shareStatusDegraded, false)
		default:
			_ = publisher.setShare(shareStatusRecovering, false)
		}
	})
}

func (r *Runtime) runShareOnce(ctx context.Context, network *Network, recorded *shareIdentity, ready func(shareIdentity)) (*shareIdentity, error) {
	attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
	client, err := r.connectGuestSSH(attempt, network)
	cancel()
	if err != nil {
		return recorded, err
	}
	// This client belongs only to the default share. Closing it must not
	// tear down browser or VNC forwards.
	defer client.Close()
	done, stop, id, err := r.shareEnvironment(ctx, client, recorded)
	if err != nil {
		if stop != nil {
			stop()
		}
		waitShare(done)
		return recorded, err
	}
	if err := r.storeShareIdentity(id); err != nil {
		stop()
		waitShare(done)
		return recorded, err
	}
	if ready != nil {
		ready(id)
	}
	select {
	case <-ctx.Done():
		stop()
		waitShare(done)
		return &id, ctx.Err()
	case shareErr := <-done:
		stop()
		if shareErr == nil {
			shareErr = errShareUnmounted
		}
		return &id, shareErr
	}
}

func waitShare(done <-chan error) {
	if done == nil {
		return
	}
	<-done
}

func (r *Runtime) hostRootPath() string {
	return filepath.Join(r.InstanceDir(), "share-root.json")
}

func (r *Runtime) loadHostRoot() (*hostRootRecord, error) {
	path := r.hostRootPath()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := host.CheckPrivateFile(path); err != nil || len(data) > 8192 {
		return nil, errShareBlocked
	}
	var record hostRootRecord
	if json.Unmarshal(data, &record) != nil || record.Path == "" || (record.Dev == 0 && record.Ino == 0) {
		return nil, errShareBlocked
	}
	return &record, nil
}

func (r *Runtime) storeHostRoot(record hostRootRecord) error {
	if record.Path == "" || (record.Dev == 0 && record.Ino == 0) {
		return errShareBlocked
	}
	return writePrivateJSON(r.hostRootPath(), record)
}

// authorizeHostRoot loads the saved host directory and opens the current
// path. A different path or inode is refused and the saved pin is left
// unchanged. The returned FileInfo is what ServeConfinedPinned must see.
func (r *Runtime) authorizeHostRoot(mountpoint string) (os.FileInfo, uint64, uint64, error) {
	saved, err := r.loadHostRoot()
	if err != nil {
		return nil, 0, 0, err
	}
	info, dev, ino, err := captureHostRoot(mountpoint)
	if err != nil {
		return nil, 0, 0, err
	}
	if dev == 0 && ino == 0 || info == nil || !info.IsDir() {
		return nil, 0, 0, errShareBlocked
	}
	if saved != nil && (saved.Path != mountpoint || saved.Dev != dev || saved.Ino != ino) {
		return nil, 0, 0, errShareBlocked
	}
	if saved == nil {
		if err := r.storeHostRoot(hostRootRecord{Path: mountpoint, Dev: dev, Ino: ino}); err != nil {
			return nil, 0, 0, err
		}
	}
	return info, dev, ino, nil
}

func (r *Runtime) shareIdentityPath() string {
	return filepath.Join(r.InstanceDir(), "share.json")
}

func (r *Runtime) loadShareIdentity() (*shareIdentity, error) {
	path := r.shareIdentityPath()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := host.CheckPrivateFile(path); err != nil || len(data) > 8192 {
		return nil, errors.New("容器项目共享身份记录无效")
	}
	var id shareIdentity
	if json.Unmarshal(data, &id) != nil || id.Mountpoint == "" || id.FSType != "fuse.sshfs" || id.Source == "" || id.MountDevice == "" {
		return nil, errors.New("容器项目共享身份记录无效")
	}
	return &id, nil
}

func (r *Runtime) storeShareIdentity(id shareIdentity) error {
	if id.FSType != "fuse.sshfs" || id.Source == "" || id.MountDevice == "" || id.Mountpoint == "" {
		return errShareBlocked
	}
	return writePrivateJSON(r.shareIdentityPath(), id)
}

func (r *Runtime) connectGuestSSH(ctx context.Context, network *Network) (*ssh.Client, error) {
	configuration, err := r.guestSSHConfiguration()
	if err != nil {
		return nil, err
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		connection, err := network.stack.DialContextTCP(attemptCtx, GuestIP+":22")
		if err == nil {
			_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
			stop := context.AfterFunc(attemptCtx, func() { _ = connection.Close() })
			transport, channels, requests, handshakeErr := ssh.NewClientConn(connection, "cage-container-"+r.identity+":22", configuration)
			stop()
			cancel()
			if handshakeErr == nil {
				_ = connection.SetDeadline(time.Time{})
				return ssh.NewClient(transport, channels, requests), nil
			}
			_ = connection.Close()
			var mismatch *knownhosts.KeyError
			if errors.As(handshakeErr, &mismatch) {
				return nil, errGuestIdentity
			}
		} else {
			cancel()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, errors.New("容器 SSH 未能就绪")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (r *Runtime) guestSSHConfiguration() (*ssh.ClientConfig, error) {
	private, err := os.ReadFile(filepath.Join(r.InstanceDir(), "id_ed25519"))
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(private)
	if err != nil {
		return nil, err
	}
	check, err := knownhosts.New(filepath.Join(r.InstanceDir(), "known_hosts"))
	if err != nil {
		return nil, err
	}
	return &ssh.ClientConfig{User: "cage", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: check, HostKeyAlgorithms: []string{ssh.KeyAlgoED25519}}, nil
}

func (r *Runtime) serveForward(ctx context.Context, network *Network, listener net.Listener, target string) {
	stopListen := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stopListen()
	dialer := &guestDialer{}
	var bridges sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		acceptForward(ctx, dialer, listener, target, &bridges)
	}()
	defer func() {
		_ = listener.Close()
		dialer.Close()
		<-acceptDone
		bridges.Wait()
	}()
	backoff := 200 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		client, err := r.connectGuestSSH(attempt, network)
		cancel()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, errGuestIdentity) {
				return
			}
			if !sleepContext(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		dialer.Set(client)
		backoff = 200 * time.Millisecond
		disconnected := make(chan struct{})
		go func() {
			_ = client.Wait()
			close(disconnected)
		}()
		select {
		case <-ctx.Done():
			_ = client.Close()
			<-disconnected
			return
		case <-disconnected:
			dialer.Clear(client)
		}
	}
}

func acceptForward(ctx context.Context, dialer *guestDialer, listener net.Listener, target string, bridges *sync.WaitGroup) {
	for {
		local, err := listener.Accept()
		if err != nil {
			return
		}
		if ctx.Err() != nil {
			local.Close()
			return
		}
		bridges.Add(1)
		go func() {
			defer bridges.Done()
			bridgeGuestConnection(ctx, dialer, local, target)
		}()
	}
}

func bridgeGuestConnection(ctx context.Context, dialer *guestDialer, local net.Conn, target string) {
	defer local.Close()
	remote, err := dialGuest(ctx, dialer, target)
	if err != nil {
		return
	}
	defer remote.Close()
	interrupt := context.AfterFunc(ctx, func() {
		_ = local.Close()
		_ = remote.Close()
		dialer.Close()
	})
	defer interrupt()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(remote, local)
		_ = remote.Close()
		_ = local.Close()
		close(done)
	}()
	_, _ = io.Copy(local, remote)
	_ = local.Close()
	_ = remote.Close()
	<-done
}

func dialGuest(ctx context.Context, dialer *guestDialer, target string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	finished := make(chan result, 1)
	go func() {
		conn, err := dialer.Dial("tcp", target)
		finished <- result{conn, err}
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		dialer.Close()
		res := <-finished
		if res.conn != nil {
			_ = res.conn.Close()
		}
		return nil, ctx.Err()
	case <-timer.C:
		dialer.Close()
		res := <-finished
		if res.conn != nil {
			_ = res.conn.Close()
		}
		return nil, context.DeadlineExceeded
	case res := <-finished:
		return res.conn, res.err
	}
}

func (r *Runtime) shareEnvironment(ctx context.Context, client *ssh.Client, recorded *shareIdentity) (<-chan error, func(), shareIdentity, error) {
	var zero shareIdentity
	mountpoint := cage.EnvironmentDir(r.profile)
	if !filepath.IsAbs(mountpoint) || strings.ContainsAny(mountpoint, "\x00\r\n") {
		return nil, nil, zero, errors.New("容器项目共享目录无效")
	}
	if err := host.EnsurePrivateDir(mountpoint); err != nil {
		return nil, nil, zero, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	mounted, current, err := r.probeGuestMount(probeCtx, client, mountpoint)
	if err != nil {
		return nil, nil, zero, err
	}
	if err := decideSharePrepare(mounted, current, recorded); err != nil {
		return nil, nil, zero, err
	}
	if mounted {
		script, scriptErr := shareUnmountScript(mountpoint, current)
		if scriptErr != nil {
			return nil, nil, zero, scriptErr
		}
		if err := r.runGuestChecked(probeCtx, client, script); err != nil {
			return nil, nil, zero, err
		}
	}
	facts, err := r.probeMountpoint(probeCtx, client, mountpoint)
	if err != nil {
		return nil, nil, zero, err
	}
	if err := decideProtect(facts, recorded); err != nil {
		return nil, nil, zero, err
	}
	expected := ""
	if recorded != nil {
		expected = recorded.UnderlyingDevice
	}
	device, err := r.protectMountpoint(probeCtx, client, mountpoint, expected)
	if err != nil {
		return nil, nil, zero, err
	}
	if recorded != nil && recorded.UnderlyingDevice != "" && device != recorded.UnderlyingDevice {
		return nil, nil, zero, errShareBlocked
	}
	rootInfo, hostDev, hostIno, err := r.authorizeHostRoot(mountpoint)
	if err != nil {
		return nil, nil, zero, err
	}
	probe, err := os.CreateTemp(mountpoint, ".cage-mount-probe-*")
	if err != nil {
		return nil, nil, zero, err
	}
	defer os.Remove(probe.Name())
	var nonce [24]byte
	if _, err = io.ReadFull(rand.Reader, nonce[:]); err != nil {
		probe.Close()
		return nil, nil, zero, err
	}
	challenge := hex.EncodeToString(nonce[:])
	_, err = probe.WriteString(challenge)
	probe.Close()
	if err != nil {
		return nil, nil, zero, err
	}
	session, err := client.NewSession()
	if err != nil {
		return nil, nil, zero, err
	}
	reader, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return nil, nil, zero, err
	}
	writer, err := session.StdinPipe()
	if err != nil {
		session.Close()
		return nil, nil, zero, err
	}
	session.Stderr = os.Stderr
	command := "exec sudo -n sshfs " + shellescape.Quote(":"+mountpoint) + " " + shellescape.Quote(mountpoint) + " -o slave,allow_other,cache=no,nodev,nosuid"
	if err = session.Start(command); err != nil {
		session.Close()
		return nil, nil, zero, err
	}
	releaseClient := context.AfterFunc(ctx, func() { _ = client.Close() })
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			releaseClient()
			_ = client.Close()
			_ = writer.Close()
			_ = session.Close()
		})
	}
	done := make(chan error, 1)
	go func() {
		err := reversesshfs.ServeConfinedPinned(&shareTransport{Reader: reader, Writer: writer, close: stop}, mountpoint, false, nil, rootInfo)
		if errors.Is(err, reversesshfs.ErrPinnedRootMismatch) {
			err = errShareBlocked
		}
		stop()
		_ = session.Wait()
		done <- err
	}()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		value, readErr := r.runGuestOutput(probeCtx, client, "cat "+shellescape.Quote(probe.Name()))
		if readErr == nil && value == challenge {
			break
		}
		if ctx.Err() != nil {
			stop()
			waitShare(done)
			return nil, nil, zero, ctx.Err()
		}
		select {
		case <-ctx.Done():
			stop()
			waitShare(done)
			return nil, nil, zero, ctx.Err()
		case <-probeCtx.Done():
			stop()
			waitShare(done)
			return nil, nil, zero, errShareUnmounted
		case <-deadline.C:
			stop()
			waitShare(done)
			return nil, nil, zero, errors.New("容器项目共享验证超时")
		case shareErr := <-done:
			stop()
			return nil, nil, zero, fmt.Errorf("容器项目共享失败: %w", shareErr)
		case <-time.After(150 * time.Millisecond):
		}
	}
	mounted, id, err := r.probeGuestMount(probeCtx, client, mountpoint)
	if err != nil {
		stop()
		waitShare(done)
		return nil, nil, zero, err
	}
	if !mounted || id.FSType != "fuse.sshfs" || id.Source == "" || id.MountDevice == "" {
		stop()
		waitShare(done)
		r.cleanupProvenMount(probeCtx, client, id)
		return nil, nil, zero, errShareBlocked
	}
	id.UnderlyingDevice = device
	id.Mountpoint = mountpoint
	id.HostDev = hostDev
	id.HostIno = hostIno
	return done, stop, id, nil
}

func (r *Runtime) cleanupProvenMount(ctx context.Context, client *ssh.Client, id shareIdentity) {
	if id.FSType != "fuse.sshfs" || id.Source == "" || id.MountDevice == "" {
		return
	}
	script, err := shareUnmountScript(id.Mountpoint, id)
	if err != nil {
		return
	}
	cleanup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = r.runGuestChecked(cleanup, client, script)
}

func (r *Runtime) probeGuestMount(ctx context.Context, client *ssh.Client, mountpoint string) (bool, shareIdentity, error) {
	output, err := r.runGuestOutput(ctx, client, shareProbeScript(mountpoint))
	if err != nil {
		return false, shareIdentity{}, err
	}
	return parseMountProbe(mountpoint, output)
}

func (r *Runtime) probeMountpoint(ctx context.Context, client *ssh.Client, mountpoint string) (mountpointFacts, error) {
	output, err := r.runGuestOutput(ctx, client, shareFactsScript(mountpoint))
	if err != nil {
		return mountpointFacts{}, err
	}
	return parseMountpointFacts(output)
}

func (r *Runtime) protectMountpoint(ctx context.Context, client *ssh.Client, mountpoint, expected string) (string, error) {
	output, err := r.runGuestOutput(ctx, client, shareProtectScript(mountpoint, expected))
	if err != nil {
		return "", err
	}
	device := strings.TrimSpace(output)
	if device == "" || strings.ContainsAny(device, "\r\n ") {
		return "", errShareBlocked
	}
	return device, nil
}

type capBuffer struct {
	buf  bytes.Buffer
	left int
}

func (c *capBuffer) Write(data []byte) (int, error) {
	if len(data) > c.left {
		return 0, errors.New("guest output limit")
	}
	n, err := c.buf.Write(data)
	c.left -= n
	return n, err
}

func (r *Runtime) runGuestOutput(ctx context.Context, client *ssh.Client, script string) (string, error) {
	stdout, _, err := r.runGuest(ctx, client, script)
	return stdout, err
}

func (r *Runtime) runGuestChecked(ctx context.Context, client *ssh.Client, script string) error {
	_, _, err := r.runGuest(ctx, client, script)
	return err
}

func (r *Runtime) runGuest(ctx context.Context, client *ssh.Client, script string) (string, string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", "", err
	}
	var stdout, stderr capBuffer
	stdout.left, stderr.left = 4096, 1024
	session.Stdout = &stdout
	session.Stderr = &stderr
	interrupt := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer interrupt()
	err = session.Run(script)
	_ = session.Close()
	if err == nil {
		return stdout.buf.String(), stderr.buf.String(), nil
	}
	var exit *ssh.ExitError
	if errors.As(err, &exit) && exit.ExitStatus() == 42 {
		return stdout.buf.String(), stderr.buf.String(), errShareBlocked
	}
	if ctx.Err() != nil {
		return stdout.buf.String(), stderr.buf.String(), ctx.Err()
	}
	detail := strings.TrimSpace(stderr.buf.String())
	if detail != "" {
		return stdout.buf.String(), detail, fmt.Errorf("容器命令失败: %w", err)
	}
	return stdout.buf.String(), "", err
}

func quotedPath(path string) (string, error) {
	if path == "" || strings.ContainsAny(path, "\x00\r\n") {
		return "", errors.New("容器项目共享路径无效")
	}
	return shellescape.Quote(path), nil
}

func shareProbeScript(mountpoint string) string {
	quoted, err := quotedPath(mountpoint)
	if err != nil {
		return "exit 42"
	}
	return `set -eu
mp=` + quoted + `
if ! findmnt -rn --mountpoint "$mp" >/dev/null 2>&1; then
  echo absent
  exit 0
fi
echo mounted
findmnt -rn -o FSTYPE --mountpoint "$mp"
findmnt -rn -o SOURCE --mountpoint "$mp"
stat -c %d:%i -- "$mp"
`
}

func shareFactsScript(mountpoint string) string {
	quoted, err := quotedPath(mountpoint)
	if err != nil {
		return "exit 42"
	}
	return `set -eu
mp=` + quoted + `
if [ -L "$mp" ]; then echo symlink; exit 0; fi
if [ ! -e "$mp" ]; then echo missing; exit 0; fi
if [ ! -d "$mp" ]; then echo notdir; exit 0; fi
nonempty=0
for entry in "$mp"/* "$mp"/.[!.]* "$mp"/..?*; do
  if [ -e "$entry" ] || [ -L "$entry" ]; then nonempty=1; break; fi
done
echo dir
echo "$nonempty"
stat -c %u -- "$mp"
stat -c %a -- "$mp"
stat -c %d:%i -- "$mp"
`
}

func shareProtectScript(mountpoint, expected string) string {
	quoted, err := quotedPath(mountpoint)
	if err != nil {
		return "exit 42"
	}
	want, err := quotedPath(expected)
	if expected == "" {
		want = "''"
		err = nil
	}
	if err != nil {
		return "exit 42"
	}
	return `set -eu
mp=` + quoted + `
expected=` + want + `
if [ -L "$mp" ]; then echo blocked >&2; exit 42; fi
if [ -e "$mp" ] && [ ! -d "$mp" ]; then echo blocked >&2; exit 42; fi
if [ -d "$mp" ]; then
  nonempty=0
  for entry in "$mp"/* "$mp"/.[!.]* "$mp"/..?*; do
    if [ -e "$entry" ] || [ -L "$entry" ]; then nonempty=1; break; fi
  done
  device=$(stat -c %d:%i -- "$mp")
  if [ "$nonempty" = 1 ] && [ "$expected" = "" ]; then echo blocked >&2; exit 42; fi
  if [ "$expected" != "" ] && [ "$device" != "$expected" ]; then echo blocked >&2; exit 42; fi
fi
sudo -n mkdir -p -- "$mp"
sudo -n chown root:root -- "$mp"
sudo -n chmod 000 -- "$mp"
if [ -L "$mp" ] || [ ! -d "$mp" ]; then echo blocked >&2; exit 42; fi
uid=$(stat -c %u -- "$mp")
mode=$(stat -c %a -- "$mp")
device=$(stat -c %d:%i -- "$mp")
if [ "$uid" != 0 ] || [ "$mode" != 0 ]; then echo blocked >&2; exit 42; fi
printf '%s\n' "$device"
`
}

func shareUnmountScript(mountpoint string, id shareIdentity) (string, error) {
	if !shareCleanupAllowed(id, id) && !(id.FSType == "fuse.sshfs" && id.Source != "" && id.MountDevice != "" && id.Mountpoint != "") {
		return "", errShareBlocked
	}
	if id.FSType != "fuse.sshfs" || id.Source == "" || id.MountDevice == "" || id.Mountpoint == "" {
		return "", errShareBlocked
	}
	mp, err := quotedPath(mountpoint)
	if err != nil {
		return "", err
	}
	source, err := quotedPath(id.Source)
	if err != nil {
		return "", err
	}
	device, err := quotedPath(id.MountDevice)
	if err != nil {
		return "", err
	}
	return `set -eu
mp=` + mp + `
want_source=` + source + `
want_device=` + device + `
if ! findmnt -rn --mountpoint "$mp" >/dev/null 2>&1; then
  exit 0
fi
fstype=$(findmnt -rn -o FSTYPE --mountpoint "$mp")
source=$(findmnt -rn -o SOURCE --mountpoint "$mp")
device=$(stat -c %d:%i -- "$mp")
if [ "$fstype" != fuse.sshfs ] || [ "$source" != "$want_source" ] || [ "$device" != "$want_device" ]; then
  echo mismatch >&2
  exit 42
fi
sudo -n fusermount3 -u -- "$mp"
if findmnt -rn --mountpoint "$mp" >/dev/null 2>&1; then
  echo still-mounted >&2
  exit 42
fi
`, nil
}

func parseMountProbe(mountpoint, output string) (bool, shareIdentity, error) {
	id := shareIdentity{Mountpoint: mountpoint}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 1 && lines[0] == "absent" {
		return false, id, nil
	}
	if len(lines) != 4 || lines[0] != "mounted" || strings.ContainsAny(lines[1]+lines[2]+lines[3], "\r") {
		return false, id, errors.New("容器项目共享挂载身份无法确认")
	}
	id.FSType = lines[1]
	id.Source = lines[2]
	id.MountDevice = lines[3]
	return true, id, nil
}

func parseMountpointFacts(output string) (mountpointFacts, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 {
		return mountpointFacts{}, errors.New("容器项目共享目录身份无法确认")
	}
	switch lines[0] {
	case "symlink":
		return mountpointFacts{Exists: true, Symlink: true}, nil
	case "missing":
		return mountpointFacts{}, nil
	case "notdir":
		return mountpointFacts{Exists: true, NonEmpty: true}, nil
	case "dir":
		if len(lines) != 5 {
			return mountpointFacts{}, errors.New("容器项目共享目录身份无法确认")
		}
		uid, err := strconv.Atoi(lines[2])
		if err != nil || strings.ContainsAny(lines[4], "\r ") {
			return mountpointFacts{}, errors.New("容器项目共享目录身份无法确认")
		}
		return mountpointFacts{Exists: true, NonEmpty: lines[1] == "1", UID: uid, Mode: lines[3], Device: lines[4]}, nil
	default:
		return mountpointFacts{}, errors.New("容器项目共享目录身份无法确认")
	}
}

type shareTransport struct {
	io.Reader
	io.Writer
	close func()
}

func (s *shareTransport) Close() error { s.close(); return nil }
