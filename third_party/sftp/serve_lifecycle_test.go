package sftp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func goroutineFrames(substr string) int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), substr)
}

func snapshotPktMgr() (controller, writer, waitIdle int) {
	return goroutineFrames("packetManager.controller"),
		goroutineFrames("packetManager.writer"),
		goroutineFrames("packetManager.waitIdle")
}

func requirePktMgrReaped(t *testing.T, controller, writer, waitIdle int) {
	t.Helper()
	gotC, gotW, gotI := snapshotPktMgr()
	if gotC != controller || gotW != writer || gotI != waitIdle {
		t.Fatalf("packet manager goroutines leaked: controller %d->%d writer %d->%d waitIdle %d->%d",
			controller, gotC, writer, gotW, waitIdle, gotI)
	}
}

func TestWriteFullShortWrites(t *testing.T) {
	pkt := &sshFxpWritePacket{
		ID:     7,
		Handle: "h",
		Offset: 99,
		Length: 11,
		Data:   []byte("hello world"),
	}
	var full bytes.Buffer
	require.NoError(t, sendPacket(&full, pkt))

	var short oneByteWriter
	require.NoError(t, sendPacket(&short, pkt))
	require.Equal(t, full.Bytes(), short.Bytes())

	err := sendPacket(zeroWriter{}, &sshFxVersionPacket{Version: 3})
	require.ErrorIs(t, err, io.ErrShortWrite)
}

type oneByteWriter struct{ bytes.Buffer }

func (w *oneByteWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := w.WriteByte(p[0]); err != nil {
		return 0, err
	}
	return 1, nil
}

type zeroWriter struct{}

func (zeroWriter) Write(p []byte) (int, error) { return 0, nil }

func TestConnCloseInterruptsBlockedWrite(t *testing.T) {
	g := &gateWriter{entered: make(chan struct{}), release: make(chan struct{})}
	c := &conn{WriteCloser: g}
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.sendPacket(&sshFxVersionPacket{Version: 3})
	}()
	select {
	case <-g.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("send did not block in Write")
	}
	closed := make(chan struct{})
	go func() {
		_ = c.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked behind Write")
	}
	select {
	case err := <-errCh:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("blocked write was not interrupted")
	}
}

// noncomparableStream is a value ReadWriteCloser with a func field, the shape
// that panicked interface equality in Close (main.stream).
type noncomparableStream struct {
	r       io.Reader
	w       io.Writer
	closeFn func() error
}

func (s noncomparableStream) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s noncomparableStream) Write(p []byte) (int, error) { return s.w.Write(p) }
func (s noncomparableStream) Close() error                { return s.closeFn() }

// sshReadCloser is not comparable (func field). projectTransport's type is
// comparable because its fields are interfaces; == still panics at runtime.
type sshReadCloser struct {
	closeFn func() error
}

func (s sshReadCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (s sshReadCloser) Close() error             { return s.closeFn() }

type projectTransport struct {
	r io.ReadCloser
	w io.WriteCloser
}

func (p projectTransport) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p projectTransport) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p projectTransport) Close() error {
	err := p.w.Close()
	if p.r != nil {
		if cerr := p.r.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

type countingPtr struct{ n *int }

func (p *countingPtr) Read([]byte) (int, error)  { return 0, io.EOF }
func (p *countingPtr) Write([]byte) (int, error) { return len([]byte{}), nil }
func (p *countingPtr) Close() error              { *p.n++; return nil }

func TestCloseNoncomparableTransportOnce(t *testing.T) {
	closes := 0
	stream := noncomparableStream{
		r: bytes.NewReader(nil),
		w: io.Discard,
		closeFn: func() error {
			closes++
			return nil
		},
	}
	// NewRequestServer stores the shared stream as transport and closes it once.
	rs := NewRequestServer(stream, InMemHandler())
	require.NoError(t, rs.Close())
	require.NoError(t, rs.Close())
	require.Equal(t, 1, closes)

	// Struct of interfaces. The type is comparable; the dynamic func closer is not.
	var wrapped int
	inner := sshReadCloser{closeFn: func() error { wrapped++; return nil }}
	pt := projectTransport{r: inner, w: nopWriteCloser{}}
	require.True(t, reflect.TypeOf(pt).Comparable())
	require.False(t, reflect.ValueOf(pt).Comparable())
	rsWrap := NewRequestServer(pt, InMemHandler())
	require.NoError(t, rsWrap.Close())
	require.Equal(t, 1, wrapped)

	// Same comparable pointer closes once.
	var ptrN int
	ptr := &countingPtr{n: &ptrN}
	cPtr := &conn{Reader: ptr, WriteCloser: ptr}
	require.NoError(t, cPtr.Close())
	require.Equal(t, 1, ptrN)

	// Same function code, different captured streams. Both must close.
	var readCloses, writeCloses int
	readS := noncomparableStream{r: bytes.NewReader(nil), w: io.Discard, closeFn: func() error { readCloses++; return nil }}
	writeS := noncomparableStream{r: bytes.NewReader(nil), w: io.Discard, closeFn: func() error { writeCloses++; return nil }}
	c2 := &conn{Reader: readS, WriteCloser: writeS}
	require.NoError(t, c2.Close())
	require.Equal(t, 1, readCloses)
	require.Equal(t, 1, writeCloses)

	// Comparable distinct pipe ends still both close.
	r, w := io.Pipe()
	cr, cw := io.Pipe()
	c3 := &conn{Reader: r, WriteCloser: cw}
	require.NoError(t, c3.Close())
	_, err := w.Write([]byte{1})
	require.Error(t, err)
	_, err = cr.Read(make([]byte, 1))
	require.Error(t, err)
}

type gateWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gateWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return 0, errors.New("write interrupted")
}

func (g *gateWriter) Close() error {
	close(g.release)
	return nil
}

func TestRequestServerCloseWithoutServe(t *testing.T) {
	beforeC, beforeW, beforeI := snapshotPktMgr()
	a, b := net.Pipe()
	t.Cleanup(func() { b.Close() })
	rs := NewRequestServer(a, InMemHandler())
	requirePktMgrReaped(t, beforeC, beforeW, beforeI)
	require.NoError(t, rs.Close())
	requirePktMgrReaped(t, beforeC, beforeW, beforeI)
	require.False(t, rs.pktMgr.running)
}

type sendErrConn struct {
	net.Conn
	err error
}

func (c *sendErrConn) Write([]byte) (int, error) { return 0, c.err }

func TestRequestServerReturnsSendError(t *testing.T) {
	beforeC, beforeW, beforeI := snapshotPktMgr()
	sentinel := errors.New("send failed")
	srv, cli := net.Pipe()
	rs := NewRequestServer(&sendErrConn{Conn: srv, err: sentinel}, InMemHandler())
	got := make(chan error, 1)
	go func() { got <- rs.Serve() }()
	go func() {
		_ = sendPacket(cli, &sshFxInitPacket{Version: 3})
		_, _ = io.Copy(io.Discard, cli)
	}()
	select {
	case err := <-got:
		require.ErrorIs(t, err, sentinel)
	case <-time.After(3 * time.Second):
		t.Fatal("Serve swallowed the send error")
	}
	require.NoError(t, cli.Close())
	requirePktMgrReaped(t, beforeC, beforeW, beforeI)
}

// writeFailEOFRead returns a real write error and makes the following read
// report io.EOF, which is what a transport close does to a blocked recv.
type writeFailEOFRead struct {
	in  *io.PipeReader
	err error
	mu  sync.Mutex
	bad bool
}

func (c *writeFailEOFRead) Read(p []byte) (int, error) {
	n, err := c.in.Read(p)
	c.mu.Lock()
	bad := c.bad
	c.mu.Unlock()
	if bad && err != nil {
		return 0, io.EOF
	}
	return n, err
}

func (c *writeFailEOFRead) Write([]byte) (int, error) {
	c.mu.Lock()
	c.bad = true
	c.mu.Unlock()
	_ = c.in.Close()
	return 0, c.err
}

func (c *writeFailEOFRead) Close() error {
	c.mu.Lock()
	c.bad = true
	c.mu.Unlock()
	return c.in.Close()
}

func TestRequestServerSendErrorWinsOverRecvEOF(t *testing.T) {
	beforeC, beforeW, beforeI := snapshotPktMgr()
	sentinel := errors.New("writer failed")
	pr, pw := io.Pipe()
	rs := NewRequestServer(&writeFailEOFRead{in: pr, err: sentinel}, InMemHandler())
	got := make(chan error, 1)
	go func() { got <- rs.Serve() }()
	require.NoError(t, sendPacket(pw, &sshFxInitPacket{Version: 3}))
	select {
	case err := <-got:
		require.ErrorIs(t, err, sentinel)
		require.False(t, errors.Is(err, io.EOF), "recv EOF hid the send error: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Serve returned EOF instead of the writer error")
	}
	_ = pw.Close()
	requirePktMgrReaped(t, beforeC, beforeW, beforeI)
}

func TestRequestServerWriteTimeoutBlockedSend(t *testing.T) {
	beforeC, beforeW, beforeI := snapshotPktMgr()
	srv, cli := net.Pipe()
	const limit = 200 * time.Millisecond
	rs := NewRequestServer(srv, InMemHandler(), WithRSWriteTimeout(limit))
	got := make(chan error, 1)
	go func() { got <- rs.Serve() }()

	require.NoError(t, sendPacket(cli, &sshFxInitPacket{Version: 3}))
	if err := readOnePacket(cli); err != nil {
		t.Fatalf("version response: %v", err)
	}
	// Idle after a completed send must not consume the deadline.
	select {
	case err := <-got:
		t.Fatalf("idle connection timed out: %v", err)
	case <-time.After(3 * limit):
	}
	require.NoError(t, sendPacket(cli, &sshFxpRealpathPacket{ID: 1, Path: "/"}))
	started := time.Now()
	select {
	case err := <-got:
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.False(t, errors.Is(err, io.EOF), "timeout surfaced as EOF: %v", err)
		if time.Since(started) > 2*time.Second {
			t.Fatalf("deadline took %s", time.Since(started))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked write was not failed by WithRSWriteTimeout")
	}
	require.Empty(t, rs.openRequests)
	_ = cli.Close()
	requirePktMgrReaped(t, beforeC, beforeW, beforeI)
}

func TestRequestServerWriteTimeoutZeroWaits(t *testing.T) {
	srv, cli := net.Pipe()
	rs := NewRequestServer(srv, InMemHandler(), WithRSWriteTimeout(0))
	got := make(chan error, 1)
	go func() { got <- rs.Serve() }()
	require.NoError(t, sendPacket(cli, &sshFxInitPacket{Version: 3}))
	select {
	case err := <-got:
		t.Fatalf("zero timeout closed a blocked write: %v", err)
	case <-time.After(80 * time.Millisecond):
	}
	require.NoError(t, rs.Close())
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not interrupt the blocked write")
	}
	_ = cli.Close()
}

func readOnePacket(r io.Reader) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	sz := binary.BigEndian.Uint32(hdr[:])
	if sz == 0 || sz > maxMsgLength {
		return errors.New("bad packet length")
	}
	_, err := io.ReadFull(r, make([]byte, sz))
	return err
}

func TestRequestServerCloseDuringBlockedWrite(t *testing.T) {
	beforeC, beforeW, beforeI := snapshotPktMgr()
	srv, cli := net.Pipe()
	entered := make(chan struct{})
	rs := NewRequestServer(&enteredConn{Conn: srv, entered: entered}, InMemHandler())
	got := make(chan error, 1)
	go func() { got <- rs.Serve() }()
	go func() {
		_ = sendPacket(cli, &sshFxInitPacket{Version: 3})
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not block in Write")
	}
	closed := make(chan error, 1)
	go func() { closed <- rs.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("RequestServer.Close blocked behind Write")
	}
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after Close")
	}
	_ = cli.Close()
	requirePktMgrReaped(t, beforeC, beforeW, beforeI)
}

type enteredConn struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
}

func (c *enteredConn) Write(p []byte) (int, error) {
	if len(p) > 0 {
		c.once.Do(func() { close(c.entered) })
	}
	return c.Conn.Write(p)
}

func TestPipelinedRequestsProgressWhileResponsesUnread(t *testing.T) {
	beforeC, beforeW, beforeI := snapshotPktMgr()
	srv, cli := net.Pipe()
	rs := NewRequestServer(srv, InMemHandler(), WithRSSequential())
	got := make(chan error, 1)
	go func() { got <- rs.Serve() }()

	const n = 32
	var buf bytes.Buffer
	require.NoError(t, sendPacket(&buf, &sshFxInitPacket{Version: 3}))
	for i := range n {
		require.NoError(t, sendPacket(&buf, &sshFxpRealpathPacket{ID: uint32(i + 1), Path: "/"}))
	}
	payload := append([]byte(nil), buf.Bytes()...)

	readErr := make(chan error, 1)
	go func() {
		for range n + 1 {
			var hdr [4]byte
			if _, err := io.ReadFull(cli, hdr[:]); err != nil {
				readErr <- err
				return
			}
			sz := binary.BigEndian.Uint32(hdr[:])
			if sz == 0 || sz > maxMsgLength {
				readErr <- errors.New("bad packet length")
				return
			}
			if _, err := io.ReadFull(cli, make([]byte, sz)); err != nil {
				readErr <- err
				return
			}
		}
		readErr <- nil
	}()
	writeErr := make(chan error, 1)
	go func() {
		_, err := cli.Write(payload)
		writeErr <- err
	}()
	select {
	case err := <-writeErr:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("pipelined write made no progress while responses were in flight")
	}
	select {
	case err := <-readErr:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("responses did not arrive for pipelined requests")
	}
	require.NoError(t, cli.Close())
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not finish after peer close")
	}
	requirePktMgrReaped(t, beforeC, beforeW, beforeI)
}

func TestRequestServerTinyBufferMixedSequential(t *testing.T) {
	beforeC, beforeW, beforeI := snapshotPktMgr()
	srvC, cliC := duplexPipe()
	t.Cleanup(func() {
		_ = srvC.Close()
		_ = cliC.Close()
	})
	rs := NewRequestServer(srvC, InMemHandler(), WithRSSequential())
	got := make(chan error, 1)
	go func() { got <- rs.Serve() }()
	client, err := NewClientPipe(cliC, cliC)
	require.NoError(t, err)
	client.maxPacket = 1024

	payload := bytes.Repeat([]byte("mixed-seq"), 8*1024)
	f, err := client.Create("/mixed")
	require.NoError(t, err)
	n, err := f.Write(payload)
	require.NoError(t, err)
	require.Equal(t, len(payload), n)
	require.NoError(t, f.Close())

	rf, err := client.Open("/mixed")
	require.NoError(t, err)
	body, err := io.ReadAll(rf)
	require.NoError(t, err)
	require.Equal(t, payload, body)
	require.NoError(t, rf.Close())
	fi, err := client.Stat("/mixed")
	require.NoError(t, err)
	require.EqualValues(t, len(payload), fi.Size())

	require.NoError(t, client.Close())
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve leaked after tiny-buffer transfer")
	}
	requirePktMgrReaped(t, beforeC, beforeW, beforeI)
}

// duplexPipe is a full-duplex stream with no kernel buffer: each Write blocks
// until the peer Read consumes it. Independent read and write goroutines must
// still make progress.
func duplexPipe() (server, client io.ReadWriteCloser) {
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()
	return &pipeDuplex{Reader: serverRead, WriteCloser: serverWrite},
		&pipeDuplex{Reader: clientRead, WriteCloser: clientWrite}
}

type pipeDuplex struct {
	io.Reader
	io.WriteCloser
}

func (p *pipeDuplex) Close() error {
	var err error
	if p.WriteCloser != nil {
		err = p.WriteCloser.Close()
	}
	if c, ok := p.Reader.(io.Closer); ok {
		if cerr := c.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

type blockingFileCmd struct {
	next    FileCmder
	entered chan struct{}
	once    sync.Once
}

func (b *blockingFileCmd) Filecmd(r *Request) error {
	if r.Method == "Mkdir" {
		b.once.Do(func() { close(b.entered) })
		<-r.Context().Done()
		return r.Context().Err()
	}
	if b.next != nil {
		return b.next.Filecmd(r)
	}
	return nil
}

func TestRequestServerCancelReapsBlockedHandler(t *testing.T) {
	beforeC, beforeW, beforeI := snapshotPktMgr()
	entered := make(chan struct{})
	h := InMemHandler()
	h.FileCmd = &blockingFileCmd{next: h.FileCmd, entered: entered}
	p := clientRequestServerPairWithHandlers(t, h)
	defer p.Close()

	mk := make(chan error, 1)
	go func() { mk <- p.cli.Mkdir("/block") }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler was not entered")
	}
	require.NoError(t, p.svr.Close())
	select {
	case err := <-p.svrResult:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after cancelling a blocked handler")
	}
	select {
	case <-mk:
	case <-time.After(3 * time.Second):
		t.Fatal("client Mkdir did not unblock")
	}
	requirePktMgrReaped(t, beforeC, beforeW, beforeI)
}
