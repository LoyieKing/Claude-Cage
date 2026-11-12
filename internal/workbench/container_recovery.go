package workbench

import (
	"context"
	"errors"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/containerenv"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

// Called under the workbench operation lock. Recover never starts a stopped
// container; the runtime's persisted intent fences Stop and stale retries.
func (s *Server) monitorContainerRuntime(ctx context.Context) error {
	if !containerenv.Configured(s.profile) {
		return nil
	}
	s.mu.Lock()
	closing, pauseVersion := s.closing, s.safety.pauseVersion
	s.mu.Unlock()
	if closing {
		return context.Canceled
	}
	runtime, err := containerenv.Load(s.profile)
	if err != nil {
		return err
	}
	inspect, cancel := context.WithTimeout(ctx, 5*time.Second)
	state, err := runtime.State(inspect)
	cancel()
	if err != nil {
		return err
	}
	if !state.Running || state.NetworkReady {
		return nil
	}
	s.mu.Lock()
	s.ready = false
	s.mu.Unlock()
	recoverCtx, finish := context.WithTimeout(ctx, 30*time.Second)
	restarted, err := runtime.Recover(recoverCtx, s.configPath, nil)
	finish()
	if err != nil || !restarted {
		return err
	}
	// Old authenticated service connections and report leases cannot cross a
	// network generation. The guest process and persistent files are untouched.
	s.controlSession.close()
	s.mu.Lock()
	s.credentials = guestCredentials{}
	s.safety.Gate = nil
	s.safety.Report = nil
	s.safety.VerifiedAt = time.Time{}
	s.recovery = networkRecovery{}
	s.mu.Unlock()
	client, err := s.gateClient()
	if err != nil {
		return err
	}
	check, end := context.WithTimeout(ctx, 5*time.Second)
	gate, err := client.Status(check)
	end()
	if err != nil {
		return err
	}
	if gate.Mode != cage.GateClosed {
		return errors.New("恢复的容器网络必须等待完整检查，不能沿用旧放行状态")
	}
	s.publishGate(gate)
	s.scheduleRecoveredContainerGate(gate, pauseVersion)
	_ = diagnostics.New(s.profile).Record("info", "runtime-recovery", "container network worker recovered; fresh egress check required")
	return nil
}

func (s *Server) scheduleRecoveredContainerGate(gate cage.GateStatus, pauseVersion uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.safety.Gate
	if s.closing || s.networkPaused || s.safety.pauseVersion != pauseVersion || gate.Mode != cage.GateClosed || gate.RunID == "" || gate.Configuration == "" || current == nil {
		return
	}
	if current.RunID != gate.RunID || current.Epoch != gate.Epoch || current.Configuration != gate.Configuration || current.Mode != cage.GateClosed {
		return
	}
	s.recovery = networkRecovery{RunID: gate.RunID, Configuration: gate.Configuration, Epoch: gate.Epoch, PauseVersion: pauseVersion, Next: time.Now()}
}
