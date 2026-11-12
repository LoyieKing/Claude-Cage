package workbench

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

func (s *Server) lifetimeContextLocked() context.Context {
	if s.lifeContext == nil {
		s.lifeContext, s.lifeCancel = context.WithCancel(context.Background())
		if s.closing {
			s.lifeCancel()
		}
	}
	return s.lifeContext
}

func (s *Server) lifetimeContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lifetimeContextLocked()
}

func (s *Server) startMonitors() {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	ctx := s.lifetimeContextLocked()
	s.monitors.Add(4)
	s.mu.Unlock()
	go func() {
		defer s.monitors.Done()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := diagnostics.New(s.profile).Clean(); err != nil {
					diagnostics.Log("error", "diagnostics", "log cleanup failed: "+err.Error())
				}
			}
		}
	}()
	go func() {
		defer s.monitors.Done()
		s.monitor(ctx)
	}()
	go func() {
		defer s.monitors.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for ctx.Err() == nil {
			// Long-running maintenance jobs own their own monitor and stop it
			// before changing guest components. Other jobs cancel observation.
			if !s.busy() {
				s.monitorSafety()
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		defer s.monitors.Done()
		// macOS performs both probes from the same tick. Its periodic SSH
		// attestation otherwise races this loop for the lifecycle lock.
		if s.macosGuest == nil && (s.windowsGuest == nil || s.windowsGuest.NetworkPairingFile != "") {
			s.monitorNetwork(ctx)
		}
	}()
}

// beginClose seals workbench admission and cancels service work without waiting.
func (s *Server) beginClose() {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	// Mark persistent attachments before cancelling their SSH transports.
	// Otherwise EOF teardown could mistake controller shutdown for user Stop.
	s.terminals.beginDetach()
	s.mu.Lock()
	s.lifetimeContextLocked()
	s.lifeCancel()
	if s.checkCancel != nil {
		s.checkCancel()
	}
	if s.monitorCancel != nil {
		s.monitorCancel()
	}
	s.mu.Unlock()
}

// Wait outside the operation lock: a cancelled job may still need that lock
// before it can finish and release its resources.
func (s *Server) closeBackground() {
	s.beginClose()
	s.jobs.close()
	s.monitors.Wait()
}

// An activity lasts through the handler's stream and child goroutine cleanup.
// Add and the closing decision share mu, so shutdown can safely wait outside it.
func (s *Server) beginActivity(parent context.Context) (context.Context, func(), error) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, nil, errJobsClosed
	}
	lifetime := s.lifetimeContextLocked()
	s.activities.Add(1)
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(lifetime, cancel)
	return ctx, func() { stop(); cancel(); s.activities.Done() }, nil
}

// closeService releases host connections while retaining the guest VM and its
// disk. It runs outside request handlers and without either workbench lock.
func (s *Server) closeService() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		// Close host attachments first to release handlers. Persistent guest
		// shells survive controller shutdown; explicit environment Stop kills them.
		s.terminals.detach()
		s.beginClose()
		s.closeBackground()
		s.activities.Wait()
		s.op.Lock()
		defer s.op.Unlock()
		s.closeProjectShares()
		s.closeWindowsBrowser()
		if s.windowsGuest != nil {
			s.closeErr = s.closeProxyGate("控制服务正在退出，外网保持关闭")
		}
		s.closeWindowsNetwork()
		s.mu.Lock()
		browser := s.browserControl
		s.ready = false
		s.credentials = guestCredentials{}
		s.mu.Unlock()
		if browser != nil {
			browser.close()
		}
		s.controlSession.close()
		if s.windowsGuest == nil {
			s.closeErr = s.closeProxyGate("控制服务正在退出，外网保持关闭")
		}
		s.closeErr = errors.Join(s.closeErr, managedproxy.Stop(s.profile))
		if s.controlPublished {
			if err := os.Remove(filepath.Join(s.profile, "control.json")); err != nil && !os.IsNotExist(err) {
				s.closeErr = errors.Join(s.closeErr, err)
			}
		}
	})
	return s.closeErr
}

func (s *Server) controlContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.lifetimeContext(), 5*time.Second)
}
