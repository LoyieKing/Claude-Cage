package workbench

import (
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func TestRecoveredContainerGateRequiresFreshMatchingGeneration(t *testing.T) {
	gate := cage.GateStatus{RunID: "new-network", Configuration: "current-config", Epoch: 3, Mode: cage.GateClosed}
	for _, sample := range []struct {
		name    string
		change  func(*Server)
		allowed bool
	}{
		{"fresh-worker", func(*Server) {}, true},
		{"manual-pause", func(s *Server) { s.networkPaused = true }, false},
		{"pause-resume-during-recovery", func(s *Server) { s.safety.pauseVersion++ }, false},
		{"shutdown", func(s *Server) { s.closing = true }, false},
		{"newer-close", func(s *Server) { s.safety.Gate.Epoch++ }, false},
		{"different-worker", func(s *Server) { s.safety.Gate.RunID = "replacement-network" }, false},
		{"changed-config", func(s *Server) { s.safety.Gate.Configuration = "replacement-config" }, false},
		{"already-granted", func(s *Server) { s.safety.Gate.Mode = cage.GateOpen }, false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			current := gate
			s := &Server{safety: safetyState{Gate: &current, pauseVersion: 8}}
			sample.change(s)
			s.scheduleRecoveredContainerGate(gate, 8)
			if !sample.allowed {
				if s.recovery.RunID != "" {
					t.Fatal("stale or paused worker scheduled egress authorization")
				}
				return
			}
			if !recoveryMatches(s.recovery, gate, gate.Configuration, false, 8) {
				t.Fatal("fresh worker did not schedule its exact closed gate for a full check")
			}
			if s.recovery.Next.After(time.Now()) || s.safety.Gate.Mode != cage.GateClosed || s.safety.Report != nil {
				t.Fatal("recovery fabricated a report, opened egress, or postponed admission")
			}
		})
	}
}
