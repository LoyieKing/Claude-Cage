package sftp

import (
	"context"
	"encoding"
	"io"
	"sort"
	"sync"
	"time"
)

// The goal of the packetManager is to keep the outgoing packets in the same
// order as the incoming as is requires by section 7 of the RFC.
//
// The controller only orders packets. A separate writer performs blocking
// sends so a stalled peer cannot stop receive, and so Close can interrupt a
// blocked write without taking the send lock. In-flight work is bounded by
// SftpServerWorkerCount admission slots released after the write finishes.

type packetManager struct {
	requests  chan orderedPacket
	responses chan orderedPacket
	sendq     chan outbound
	admit     chan struct{}

	done chan struct{}
	fini chan struct{}

	incoming orderedPackets
	outgoing orderedPackets

	sender  packetSender
	working sync.WaitGroup

	packetCount uint32
	alloc       *allocator
	// sequential processes every packet in a single worker, in the order received
	sequential bool
	// writeTimeout is how long one response send may block. Zero disables it.
	writeTimeout time.Duration

	onFatal func(error)

	mu      sync.Mutex // protects running, shut
	errMu   sync.Mutex
	fatal   error
	running bool
	shut    bool

	stopOnce sync.Once
	shutOnce sync.Once
	sendOnce sync.Once
	failOnce sync.Once

	wtMu    sync.Mutex
	wt      *time.Timer
	wtGen   uint64
	wtFired bool

	controllerDone chan struct{}
	writerDone     chan struct{}
	dispatchDone   chan struct{}
}

type packetSender interface {
	sendPacket(encoding.BinaryMarshaler) error
}

func newPktMgr(sender packetSender) *packetManager {
	n := SftpServerWorkerCount
	if n < 1 {
		n = 1
	}
	return &packetManager{
		requests:       make(chan orderedPacket, n),
		responses:      make(chan orderedPacket, n),
		sendq:          make(chan outbound, 1),
		admit:          make(chan struct{}, n),
		done:           make(chan struct{}),
		fini:           make(chan struct{}),
		incoming:       make([]orderedPacket, 0, n),
		outgoing:       make([]orderedPacket, 0, n),
		sender:         sender,
		controllerDone: make(chan struct{}),
		writerDone:     make(chan struct{}),
	}
}

// start launches the ordering controller and the blocking writer once.
// Construction does not start them: NewRequestServer followed by Close
// without Serve must not leak goroutines. shutdown/close are safe if start
// never ran.
func (s *packetManager) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startLocked()
}

func (s *packetManager) startLocked() {
	if s.shut || s.running {
		return
	}
	s.running = true
	go s.controller()
	go s.writer()
}

// // packet ordering
func (s *packetManager) newOrderID() uint32 {
	s.packetCount++
	return s.packetCount
}

// returns the next orderID without incrementing it.
// This is used before receiving a new packet, with the allocator enabled, to associate
// the slice allocated for the received packet with the orderID that will be used to mark
// the allocated slices for reuse once the request is served
func (s *packetManager) getNextOrderID() uint32 {
	return s.packetCount + 1
}

type admission struct {
	once sync.Once
	pm   *packetManager
}

func (a *admission) release() {
	if a == nil || a.pm == nil {
		return
	}
	a.once.Do(func() {
		select {
		case <-a.pm.admit:
		default:
		}
	})
}

type orderedRequest struct {
	requestPacket
	orderid uint32
	adm     *admission
}

func (s *packetManager) newOrderedRequest(p requestPacket) orderedRequest {
	return orderedRequest{requestPacket: p, orderid: s.newOrderID()}
}
func (p orderedRequest) orderID() uint32 { return p.orderid }

type orderedResponse struct {
	responsePacket
	orderid uint32
}

func (s *packetManager) newOrderedResponse(p responsePacket, id uint32,
) orderedResponse {
	return orderedResponse{responsePacket: p, orderid: id}
}
func (p orderedResponse) orderID() uint32 { return p.orderid }

type orderedPacket interface {
	id() uint32
	orderID() uint32
}
type orderedPackets []orderedPacket

func (o orderedPackets) Sort() {
	sort.Slice(o, func(i, j int) bool {
		// The difference, not the values: orderIDs wrap around after 2^32 packets.
		return int32(o[i].orderID()-o[j].orderID()) < 0
	})
}

type outbound struct {
	pkt orderedPacket
	adm *admission
}

// // packet registry
// register incoming packets to be handled. Returns false if the session is stopping.
func (s *packetManager) incomingPacket(pkt orderedRequest) bool {
	s.start()
	s.working.Add(1)
	select {
	case s.requests <- pkt:
		return true
	case <-s.done:
		s.working.Done()
		return false
	}
}

// register outgoing packets as being ready
func (s *packetManager) readyPacket(pkt orderedResponse) {
	s.start()
	select {
	case s.responses <- pkt:
	case <-s.done:
	}
	s.working.Done()
}

// drop releases an admitted packet that will not be answered (cancellation).
func (s *packetManager) drop(pkt orderedRequest) {
	pkt.adm.release()
	s.working.Done()
}

func (s *packetManager) requestStop() {
	s.stopOnce.Do(func() { close(s.done) })
}

func (s *packetManager) recordFatal(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	if s.fatal == nil {
		s.fatal = err
	}
	s.errMu.Unlock()
}

func (s *packetManager) fatalErr() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.fatal
}

// fail records the first session error, stops admission, and closes the
// transport so a blocked read or write unblocks. The recorded error is the
// one Serve returns (a later EOF must not hide it).
func (s *packetManager) fail(err error) {
	if err == nil {
		return
	}
	s.recordFatal(err)
	s.requestStop()
	s.failOnce.Do(func() {
		if cl, ok := s.sender.(io.Closer); ok {
			_ = cl.Close()
		}
		if s.onFatal != nil {
			s.onFatal(err)
		}
	})
}

// close flushes in-order responses and reaps the controller and writer.
// It is the clean end of a dispatch loop (peer EOF or tests).
func (s *packetManager) close() {
	s.mu.Lock()
	if !s.running && !s.shut {
		s.startLocked()
	}
	running := s.running
	s.mu.Unlock()
	if !running {
		s.markShut()
		return
	}
	s.working.Wait()
	s.shutOnce.Do(func() {
		s.mu.Lock()
		s.shut = true
		s.mu.Unlock()
		close(s.fini)
		<-s.controllerDone
		<-s.writerDone
	})
}

// shutdown stops a session that may never have been started and reaps any
// controller/writer. Safe to call more than once and after close.
func (s *packetManager) shutdown() {
	s.requestStop()
	s.shutOnce.Do(func() {
		s.mu.Lock()
		running := s.running
		s.shut = true
		s.mu.Unlock()
		if !running {
			return
		}
		<-s.controllerDone
		<-s.writerDone
	})
}

func (s *packetManager) markShut() {
	s.shutOnce.Do(func() {
		s.mu.Lock()
		s.shut = true
		s.mu.Unlock()
	})
}

func (s *packetManager) acquire(a *admission) bool {
	select {
	case s.admit <- struct{}{}:
		return true
	case <-s.done:
		return false
	}
}

func (s *packetManager) enqueueWorker(ch chan<- orderedRequest, pkt orderedRequest) bool {
	if !s.incomingPacket(pkt) {
		pkt.adm.release()
		return false
	}
	select {
	case ch <- pkt:
		return true
	case <-s.done:
		s.working.Done()
		pkt.adm.release()
		return false
	}
}

func (s *packetManager) waitIdle() bool {
	finished := make(chan struct{})
	go func() {
		s.working.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		return true
	case <-s.done:
		return false
	}
}

// Passed a worker function, returns a channel for incoming packets.
// Keep process packet responses in the order they are received while
// maximizing throughput of file transfers.
func (s *packetManager) workerChan(runWorker func(chan orderedRequest),
) chan orderedRequest {
	s.start()
	pktChan := make(chan orderedRequest, SftpServerWorkerCount)
	done := make(chan struct{})
	s.mu.Lock()
	s.dispatchDone = done
	s.mu.Unlock()

	if s.sequential {
		ch := make(chan orderedRequest)
		runWorker(ch)
		go s.serveDispatch(done, pktChan, nil, ch)
		return pktChan
	}

	// multiple workers for faster read/writes
	rwChan := make(chan orderedRequest, SftpServerWorkerCount)
	for range SftpServerWorkerCount {
		runWorker(rwChan)
	}

	// single worker to enforce sequential processing of everything else
	cmdChan := make(chan orderedRequest)
	runWorker(cmdChan)
	go s.serveDispatch(done, pktChan, rwChan, cmdChan)
	return pktChan
}

func (s *packetManager) serveDispatch(done chan struct{}, pktChan, rwChan, cmdChan chan orderedRequest) {
	// Flush sends before closing worker channels. Server.Serve only waits for
	// workers, so allocator pages must already have been released.
	defer close(done)
	if rwChan != nil {
		defer close(rwChan)
	}
	defer close(cmdChan)
	defer s.close()
	sequential := rwChan == nil
	for {
		select {
		case <-s.done:
			return
		case pkt, ok := <-pktChan:
			if !ok {
				return
			}
			if !s.dispatchOne(pkt, rwChan, cmdChan, sequential) {
				return
			}
		}
	}
}

func (s *packetManager) dispatchOne(pkt orderedRequest, rwChan, cmdChan chan orderedRequest, sequential bool) bool {
	pkt.adm = &admission{pm: s}
	if !s.acquire(pkt.adm) {
		return false
	}
	if sequential {
		return s.enqueueWorker(cmdChan, pkt)
	}
	switch p := pkt.requestPacket.(type) {
	case *sshFxpReadPacket, *sshFxpWritePacket:
		return s.enqueueWorker(rwChan, pkt)
	case *sshFxpClosePacket:
		// wait for reads/writes to finish when file is closed
		// incomingPacket() call must occur after this
		if !s.waitIdle() {
			pkt.adm.release()
			return false
		}
		return s.enqueueWorker(cmdChan, pkt)
	case *sshFxpExtendedPacket:
		// and before syncing the file they wrote
		if _, ok := p.SpecificPacket.(*sshFxpFsyncPacket); ok {
			if !s.waitIdle() {
				pkt.adm.release()
				return false
			}
		}
		return s.enqueueWorker(cmdChan, pkt)
	default:
		return s.enqueueWorker(cmdChan, pkt)
	}
}

func (s *packetManager) noteIn(pkt orderedPacket) {
	debug("incoming id (oid): %v (%v)", pkt.id(), pkt.orderID())
	s.incoming = append(s.incoming, pkt)
	s.incoming.Sort()
}

func (s *packetManager) noteOut(pkt orderedPacket) {
	debug("outgoing id (oid): %v (%v)", pkt.id(), pkt.orderID())
	s.outgoing = append(s.outgoing, pkt)
	s.outgoing.Sort()
}

func popPacket(o orderedPackets) orderedPackets {
	copy(o, o[1:])
	o[len(o)-1] = nil
	return o[:len(o)-1]
}

func (s *packetManager) popMatch() *outbound {
	if len(s.outgoing) == 0 || len(s.incoming) == 0 {
		return nil
	}
	out := s.outgoing[0]
	in := s.incoming[0]
	if in.orderID() != out.orderID() {
		return nil
	}
	var adm *admission
	if req, ok := in.(orderedRequest); ok {
		adm = req.adm
	}
	s.incoming = popPacket(s.incoming)
	s.outgoing = popPacket(s.outgoing)
	return &outbound{pkt: out, adm: adm}
}

func (s *packetManager) drainInputs() {
	for {
		select {
		case pkt := <-s.requests:
			s.noteIn(pkt)
		case pkt := <-s.responses:
			s.noteOut(pkt)
		default:
			return
		}
	}
}

func (s *packetManager) releaseIncoming() {
	for i, pkt := range s.incoming {
		if req, ok := pkt.(orderedRequest); ok {
			req.adm.release()
		}
		s.incoming[i] = nil
	}
	s.incoming = nil
}

func (s *packetManager) closeSendq() {
	s.sendOnce.Do(func() { close(s.sendq) })
}

// process packets. Never calls the transport.
func (s *packetManager) controller() {
	defer close(s.controllerDone)
	var pending *outbound
	defer func() {
		if pending != nil {
			select {
			case s.sendq <- *pending:
			default:
				pending.adm.release()
			}
			pending = nil
		}
		s.releaseIncoming()
		s.closeSendq()
	}()
	flushing := false
	for {
		if pending == nil {
			pending = s.popMatch()
		}
		if flushing && pending == nil && len(s.requests) == 0 && len(s.responses) == 0 {
			return
		}
		var outCh chan outbound
		var send outbound
		if pending != nil {
			outCh = s.sendq
			// The select statement evaluates every send expression on entry,
			// including cases whose channel is nil. Copy first so a nil
			// pending is never dereferenced.
			send = *pending
		}
		select {
		case pkt := <-s.requests:
			s.noteIn(pkt)
		case pkt := <-s.responses:
			s.noteOut(pkt)
		case outCh <- send:
			debug("Sending packet: %v", pending.pkt.id())
			pending = nil
		case <-s.done:
			return
		case <-s.fini:
			flushing = true
			s.drainInputs()
		}
	}
}

// writer is the only goroutine that blocks in sendPacket.
// Allocator pages stay owned by the request until this returns.
func (s *packetManager) writer() {
	defer close(s.writerDone)
	defer s.disarmWriteTimeout()
	for ob := range s.sendq {
		if ob.pkt == nil {
			ob.adm.release()
			continue
		}
		s.armWriteTimeout()
		err := s.sender.sendPacket(ob.pkt.(encoding.BinaryMarshaler))
		if s.writeDeadlineFired() {
			err = context.DeadlineExceeded
		}
		s.disarmWriteTimeout()
		if s.alloc != nil {
			s.alloc.ReleasePages(ob.pkt.orderID())
		}
		ob.adm.release()
		if err != nil {
			s.fail(err)
		}
	}
}

// armWriteTimeout starts the single session timer for this blocked send.
// A completed send disarms it, so an idle connection is not timed out and
// each packet gets a fresh deadline. No extra goroutine is started per packet.
func (s *packetManager) armWriteTimeout() {
	if s.writeTimeout <= 0 {
		return
	}
	s.wtMu.Lock()
	defer s.wtMu.Unlock()
	s.wtGen++
	s.wtFired = false
	gen := s.wtGen
	if s.wt != nil {
		s.wt.Stop()
	}
	s.wt = time.AfterFunc(s.writeTimeout, func() {
		s.wtMu.Lock()
		fire := s.wtGen == gen && !s.wtFired
		if fire {
			s.wtFired = true
		}
		s.wtMu.Unlock()
		if fire {
			s.fail(context.DeadlineExceeded)
		}
	})
}

func (s *packetManager) disarmWriteTimeout() {
	if s.writeTimeout <= 0 {
		return
	}
	s.wtMu.Lock()
	defer s.wtMu.Unlock()
	s.wtGen++
	if s.wt != nil {
		s.wt.Stop()
		s.wt = nil
	}
}

func (s *packetManager) writeDeadlineFired() bool {
	if s.writeTimeout <= 0 {
		return false
	}
	s.wtMu.Lock()
	defer s.wtMu.Unlock()
	return s.wtFired
}
