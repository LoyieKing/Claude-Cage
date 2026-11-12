package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	rdebug "runtime/debug"
	"strconv"
	"sync"
	"time"
)

const defaultMaxTxPacket uint32 = 1 << 15

// Handlers contains the 4 SFTP server request handlers.
type Handlers struct {
	FileGet  FileReader
	FilePut  FileWriter
	FileCmd  FileCmder
	FileList FileLister
}

// RequestServer abstracts the sftp protocol with an http request-like protocol
type RequestServer struct {
	Handlers Handlers

	*serverConn
	pktMgr *packetManager

	startDirectory string
	maxTxPacket    uint32
	maxHandles     int

	mu           sync.RWMutex
	handleCount  int
	openRequests map[string]*Request

	lifeMu  sync.Mutex
	cancel  context.CancelFunc
	started bool // Serve has taken responsibility for reaping pktMgr
	closed  bool
}

// A RequestServerOption is a function which applies configuration to a RequestServer.
type RequestServerOption func(*RequestServer)

// WithRSAllocator enable the allocator.
// After processing a packet we keep in memory the allocated slices
// and we reuse them for new packets.
// The allocator is experimental
func WithRSAllocator() RequestServerOption {
	return func(rs *RequestServer) {
		alloc := newAllocator()
		rs.pktMgr.alloc = alloc
		rs.conn.alloc = alloc
	}
}

// WithStartDirectory sets a start directory to use as base for relative paths.
// If unset the default is "/"
func WithStartDirectory(startDirectory string) RequestServerOption {
	return func(rs *RequestServer) {
		rs.startDirectory = cleanPath(startDirectory)
	}
}

// WithRSMaxTxPacket sets the maximum size of the payload returned to the client,
// measured in bytes. The default value is 32768 bytes, and this option
// can only be used to increase it. Setting this option to a larger value
// should be safe, because the client decides the size of the requested payload.
//
// The default maximum packet size is 32768 bytes.
func WithRSMaxTxPacket(size uint32) RequestServerOption {
	return func(rs *RequestServer) {
		if size < defaultMaxTxPacket {
			return
		}

		rs.maxTxPacket = size
	}
}

// WithRSMaxHandles makes OPEN and OPENDIR fail once n handles are open.
// By default there is no limit, and each handle may hold a file descriptor and memory.
func WithRSMaxHandles(n int) RequestServerOption {
	return func(rs *RequestServer) {
		rs.maxHandles = n
	}
}

// WithRSSequential processes requests one at a time, in the order they are received.
//
// By default, reads and writes run concurrently with each other and with the other
// requests, so a request can see or undo the effect of a write sent before it:
// two writes to the same range can land in either order, and a FSTAT or FSETSTAT
// sent after a WRITE can run before it. OpenSSH's sftp-server is sequential.
func WithRSSequential() RequestServerOption {
	return func(rs *RequestServer) {
		rs.pktMgr.sequential = true
	}
}

// WithRSWriteTimeout bounds one response packet write by d. The deadline runs
// only during that send and resets when the send completes; partial writes
// within the same packet do not extend it. Idle reads are not closed. Serve returns
// context.DeadlineExceeded (not io.EOF) and the underlying stream is closed,
// which unblocks both a blocked Write and a blocked Read. d <= 0 disables the
// deadline and keeps the historical wait-until-Close behavior. Apply this
// option to NewRequestServer before Serve, the same way as WithRSSequential.
// Confined servers should pass their own limit (typically 30s); the library
// default stays off for compatibility.
func WithRSWriteTimeout(d time.Duration) RequestServerOption {
	return func(rs *RequestServer) {
		if d < 0 {
			d = 0
		}
		rs.pktMgr.writeTimeout = d
	}
}

// NewRequestServer creates/allocates/returns new RequestServer.
// Normally there will be one server per user-session.
func NewRequestServer(rwc io.ReadWriteCloser, h Handlers, options ...RequestServerOption) *RequestServer {
	svrConn := &serverConn{
		conn: conn{
			Reader:      rwc,
			WriteCloser: rwc,
			transport:   rwc,
		},
	}
	rs := &RequestServer{
		Handlers: h,

		serverConn: svrConn,
		pktMgr:     newPktMgr(svrConn),

		startDirectory: "/",
		maxTxPacket:    defaultMaxTxPacket,

		openRequests: make(map[string]*Request),
	}

	for _, o := range options {
		o(rs)
	}
	return rs
}

// New Open packet/Request
func (rs *RequestServer) nextRequest(r *Request) string {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	rs.handleCount++

	r.handle = strconv.Itoa(rs.handleCount)
	rs.openRequests[r.handle] = r

	return r.handle
}

// handleLimitReached reports whether no more handle may be opened (see WithRSMaxHandles).
func (rs *RequestServer) handleLimitReached() bool {
	if rs.maxHandles <= 0 {
		return false
	}
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return len(rs.openRequests) >= rs.maxHandles
}

var errTooManyHandles = errors.New("too many open handles")

// Returns Request from openRequests, bool is false if it is missing.
//
// The Requests in openRequests work essentially as open file descriptors that
// you can do different things with. What you are doing with it are denoted by
// the first packet of that type (read/write/etc).
func (rs *RequestServer) getRequest(handle string) (*Request, bool) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	r, ok := rs.openRequests[handle]
	return r, ok
}

// Close the Request and clear from openRequests map
func (rs *RequestServer) closeRequest(handle string) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if r, ok := rs.openRequests[handle]; ok {
		delete(rs.openRequests, handle)
		return r.close()
	}

	return EBADF
}

// Close interrupts a blocked send without taking the send lock, cancels
// in-flight handlers, and reaps the packet manager when Serve never started.
func (rs *RequestServer) Close() error {
	rs.lifeMu.Lock()
	rs.closed = true
	started := rs.started
	cancel := rs.cancel
	rs.lifeMu.Unlock()
	if cancel != nil {
		cancel()
	}
	err := rs.conn.Close()
	rs.pktMgr.requestStop()
	if !started {
		rs.pktMgr.shutdown()
	}
	return err
}

func (rs *RequestServer) serveLoop(ctx context.Context, pktChan chan<- orderedRequest) error {
	defer close(pktChan) // shuts down sftpServerWorkers

	var err error
	var pkt requestPacket
	var pktType fxp
	var pktBytes []byte

	for {
		pktType, pktBytes, err = rs.serverConn.recvPacket(rs.pktMgr.getNextOrderID())
		if err != nil {
			// we don't care about releasing allocated pages here, the server will quit and the allocator freed
			return err
		}

		pkt, err = makePacket(rxPacket{pktType, pktBytes})
		if err != nil {
			switch {
			case errors.Is(err, errUnknownExtendedPacket):
				// do nothing
			default:
				debug("makePacket err: %v", err)
				return err
			}
		}

		req := rs.pktMgr.newOrderedRequest(pkt)
		select {
		case pktChan <- req:
		case <-ctx.Done():
			return ctx.Err()
		case <-rs.pktMgr.done:
			if ferr := rs.pktMgr.fatalErr(); ferr != nil {
				return ferr
			}
			return io.ErrClosedPipe
		}
	}
}

// Serve requests for user session
func (rs *RequestServer) Serve() error {
	defer func() {
		if rs.pktMgr.alloc != nil {
			rs.pktMgr.alloc.Free()
		}
	}()

	rs.lifeMu.Lock()
	if rs.closed {
		rs.lifeMu.Unlock()
		return io.ErrClosedPipe
	}
	ctx, cancel := context.WithCancel(context.Background())
	rs.cancel = cancel
	rs.started = true
	rs.lifeMu.Unlock()
	defer cancel()

	// Writer failures cancel handlers. fail also closes the transport.
	rs.pktMgr.onFatal = func(error) { cancel() }

	var wg sync.WaitGroup
	runWorker := func(ch chan orderedRequest) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := rs.packetWorker(ctx, ch); err != nil {
				rs.pktMgr.fail(err)
			}
		}()
	}
	pktChan := rs.pktMgr.workerChan(runWorker)

	err := rs.serveLoop(ctx, pktChan)
	// Clean EOF stays EOF so open handles can convert it. Any other
	// read/protocol error cancels the session. A send error recorded by
	// the writer wins over the read error caused by closing the transport.
	if err != nil && !errors.Is(err, io.EOF) {
		rs.pktMgr.fail(err)
	}

	wg.Wait() // workers finish before open-handle cleanup
	if rs.pktMgr.dispatchDone != nil {
		<-rs.pktMgr.dispatchDone
	}
	rs.pktMgr.shutdown()

	// Closing the transport after a failed send often makes the blocked read
	// return io.EOF. That EOF is a consequence of the send failure, not a
	// clean peer shutdown, so the send error wins.
	if ferr := rs.pktMgr.fatalErr(); ferr != nil {
		err = ferr
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()

	// make sure all open requests are properly closed
	// (eg. possible on dropped connections, client crashes, etc.)
	for handle, req := range rs.openRequests {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		req.transferError(err)

		delete(rs.openRequests, handle)
		req.close()
	}

	return err
}

func (rs *RequestServer) packetWorker(ctx context.Context, pktChan chan orderedRequest) error {
	for pkt := range pktChan {
		if ctx.Err() != nil {
			rs.pktMgr.drop(pkt)
			continue
		}
		orderID := pkt.orderID()
		if epkt, ok := pkt.requestPacket.(*sshFxpExtendedPacket); ok {
			if epkt.SpecificPacket != nil {
				pkt.requestPacket = epkt.SpecificPacket
			}
		}

		rpkt := rs.handlePacket(ctx, pkt.requestPacket, orderID)

		rs.pktMgr.readyPacket(
			rs.pktMgr.newOrderedResponse(rpkt, orderID))
	}
	return nil
}

// handlePacket answers one packet. A panic in a handler fails that request, instead of
// the whole process, which may serve other connections or, as in Lima, run the VM.
func (rs *RequestServer) handlePacket(ctx context.Context, pkt requestPacket, orderID uint32) (rpkt responsePacket) {
	defer func() {
		if p := recover(); p != nil {
			debug("handlePacket: panic: %v\n%s", p, rdebug.Stack())
			rpkt = statusFromError(pkt.id(), fmt.Errorf("sftp server: internal error: %v", p))
		}
	}()

	switch pkt := pkt.(type) {
	case *sshFxInitPacket:
		rpkt = &sshFxVersionPacket{Version: sftpProtocolVersion, Extensions: sftpExtensions}
	case *sshFxpClosePacket:
		handle := pkt.getHandle()
		rpkt = statusFromError(pkt.ID, rs.closeRequest(handle))
	case *sshFxpRealpathPacket:
		var realPath string
		var err error

		switch pather := rs.Handlers.FileList.(type) {
		case RealPathFileLister:
			realPath, err = pather.RealPath(pkt.getPath())
		case legacyRealPathFileLister:
			realPath = pather.RealPath(pkt.getPath())
		default:
			realPath = cleanPathWithBase(rs.startDirectory, pkt.getPath())
		}
		if err != nil {
			rpkt = statusFromError(pkt.ID, err)
		} else {
			rpkt = cleanPacketPath(pkt, realPath)
		}
	case *sshFxpFsyncPacket:
		request, ok := rs.getRequest(pkt.Handle)
		if !ok {
			rpkt = statusFromError(pkt.ID, EBADF)
		} else if s, ok := request.opened().(interface{ Sync() error }); ok {
			rpkt = statusFromError(pkt.ID, s.Sync())
		} else {
			rpkt = statusFromError(pkt.ID, ErrSSHFxOpUnsupported)
		}
	case *sshFxpOpendirPacket:
		if rs.handleLimitReached() {
			rpkt = statusFromError(pkt.ID, errTooManyHandles)
			break
		}
		request := requestFromPacket(ctx, pkt, rs.startDirectory)
		handle := rs.nextRequest(request)
		rpkt = request.opendir(rs.Handlers, pkt)
		if _, ok := rpkt.(*sshFxpHandlePacket); !ok {
			// if we return an error we have to remove the handle from the active ones
			rs.closeRequest(handle)
		}
	case *sshFxpOpenPacket:
		if rs.handleLimitReached() {
			rpkt = statusFromError(pkt.ID, errTooManyHandles)
			break
		}
		request := requestFromPacket(ctx, pkt, rs.startDirectory)
		handle := rs.nextRequest(request)
		rpkt = request.open(rs.Handlers, pkt)
		if _, ok := rpkt.(*sshFxpHandlePacket); !ok {
			// if we return an error we have to remove the handle from the active ones
			rs.closeRequest(handle)
		}
	case *sshFxpFstatPacket:
		handle := pkt.getHandle()
		request, ok := rs.getRequest(handle)
		if !ok {
			rpkt = statusFromError(pkt.ID, EBADF)
		} else {
			request = &Request{
				Method:     "Stat",
				Filepath:   cleanPathWithBase(rs.startDirectory, request.Filepath),
				handleFile: request.opened(),
			}
			rpkt = request.call(rs.Handlers, pkt, rs.pktMgr.alloc, orderID, rs.maxTxPacket)
		}
	case *sshFxpFsetstatPacket:
		handle := pkt.getHandle()
		request, ok := rs.getRequest(handle)
		if !ok {
			rpkt = statusFromError(pkt.ID, EBADF)
		} else {
			request = &Request{
				Method:     "Setstat",
				Filepath:   cleanPathWithBase(rs.startDirectory, request.Filepath),
				handleFile: request.opened(),
			}
			rpkt = request.call(rs.Handlers, pkt, rs.pktMgr.alloc, orderID, rs.maxTxPacket)
		}
	case *sshFxpExtendedPacketPosixRename:
		request := &Request{
			Method:   "PosixRename",
			Filepath: cleanPathWithBase(rs.startDirectory, pkt.Oldpath),
			Target:   cleanPathWithBase(rs.startDirectory, pkt.Newpath),
		}
		rpkt = request.call(rs.Handlers, pkt, rs.pktMgr.alloc, orderID, rs.maxTxPacket)
	case *sshFxpExtendedPacketStatVFS:
		request := &Request{
			Method:   "StatVFS",
			Filepath: cleanPathWithBase(rs.startDirectory, pkt.Path),
		}
		rpkt = request.call(rs.Handlers, pkt, rs.pktMgr.alloc, orderID, rs.maxTxPacket)
	case hasHandle:
		handle := pkt.getHandle()
		request, ok := rs.getRequest(handle)
		if !ok {
			rpkt = statusFromError(pkt.id(), EBADF)
		} else {
			rpkt = request.call(rs.Handlers, pkt, rs.pktMgr.alloc, orderID, rs.maxTxPacket)
		}
	case hasPath:
		request := requestFromPacket(ctx, pkt, rs.startDirectory)
		rpkt = request.call(rs.Handlers, pkt, rs.pktMgr.alloc, orderID, rs.maxTxPacket)
		request.close()
	default:
		rpkt = statusFromError(pkt.id(), ErrSSHFxOpUnsupported)
	}
	return rpkt
}

// clean and return name packet for file
func cleanPacketPath(pkt *sshFxpRealpathPacket, realPath string) responsePacket {
	return &sshFxpNamePacket{
		ID: pkt.id(),
		NameAttrs: []*sshFxpNameAttr{
			{
				Name:     realPath,
				LongName: realPath,
				Attrs:    emptyFileStat,
			},
		},
	}
}

// Makes sure we have a clean POSIX (/) absolute path to work with
func cleanPath(p string) string {
	return cleanPathWithBase("/", p)
}

func cleanPathWithBase(base, p string) string {
	p = filepath.ToSlash(filepath.Clean(p))
	if !path.IsAbs(p) {
		return path.Join(base, p)
	}
	return p
}
