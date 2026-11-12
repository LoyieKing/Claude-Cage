package containerenv

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func TestPlanRecoveryNeverRestartsStoppedOrLiveWorkers(t *testing.T) {
	base := recoveryFacts{Desired: intentRunning, SnapshotGeneration: 4, CurrentGeneration: 4, ContainerExists: true, ContainerRunning: true, MountsReady: true, Rootless: true, ResourcesVerified: true, ConfigMatches: true}
	cases := []struct {
		name    string
		change  func(*recoveryFacts)
		restart bool
		blocked bool
	}{
		{name: "dead worker", change: func(*recoveryFacts) {}, restart: true},
		{name: "share is not a reason", change: func(*recoveryFacts) {}, restart: true},
		{name: "worker alive", change: func(f *recoveryFacts) { f.WorkerAlive = true }},
		{name: "lock held", change: func(f *recoveryFacts) { f.NetworkLockHeld = true }},
		{name: "backoff", change: func(f *recoveryFacts) { f.BackingOff = true }},
		{name: "stopped desire", change: func(f *recoveryFacts) { f.Desired = intentStopped }},
		{name: "stale generation", change: func(f *recoveryFacts) { f.CurrentGeneration = 5 }},
		{name: "container stopped", change: func(f *recoveryFacts) { f.ContainerRunning = false }},
		{name: "container absent", change: func(f *recoveryFacts) { f.ContainerExists = false; f.ContainerRunning = false }},
		{name: "rootless mismatch", change: func(f *recoveryFacts) { f.Rootless = false }, blocked: true},
		{name: "resource mismatch", change: func(f *recoveryFacts) { f.ResourcesVerified = false }, blocked: true},
		{name: "mounts not ready", change: func(f *recoveryFacts) { f.MountsReady = false }, blocked: true},
		{name: "config mismatch", change: func(f *recoveryFacts) { f.ConfigMatches = false }, blocked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts := base
			tc.change(&facts)
			restart, err := planNetworkRecovery(facts)
			if restart != tc.restart || (err != nil) != tc.blocked {
				t.Fatalf("restart=%t err=%v", restart, err)
			}
		})
	}
}

func TestNetworkReadyDoesNotRequireDefaultShare(t *testing.T) {
	degraded := State{ResourcesVerified: true, MountsReady: true}
	applyNetworkRecord(&degraded, networkRecord{Shared: false, ShareStatus: shareStatusDegraded, NetworkStatus: networkStatusReady, BrowserAddress: "127.0.0.1:2000", VNCAddress: "127.0.0.1:2001"})
	if !degraded.NetworkReady || degraded.Shared || !degraded.Ready || degraded.ShareStatus != shareStatusDegraded {
		t.Fatalf("degraded share changed network readiness: %+v", degraded)
	}
	unverified := State{}
	applyNetworkRecord(&unverified, networkRecord{Shared: false, ShareStatus: shareStatusRecovering})
	if !unverified.NetworkReady || unverified.Ready || unverified.Shared {
		t.Fatalf("network liveness was hidden or overstated: %+v", unverified)
	}
	legacy := networkRecord{Shared: true}
	if !shareReady(legacy) {
		t.Fatal("legacy share record was dropped")
	}
	ready := State{ResourcesVerified: true, MountsReady: true}
	applyNetworkRecord(&ready, networkRecord{Shared: true, ShareStatus: shareStatusReady})
	if !ready.Shared || !ready.NetworkReady {
		t.Fatal("verified share was not reported")
	}
}

func TestStopIntentBlocksLateRecovery(t *testing.T) {
	profile := t.TempDir()
	runtime, err := Configure(profile, fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.advanceIntent(intentRunning, "fingerprint")
	if err != nil {
		t.Fatal(err)
	}
	hold := make(chan struct{})
	release := make(chan struct{})
	go func() {
		unlock, lockErr := lockRuntime(context.Background(), filepath.Join(runtime.InstanceDir(), "lifecycle.lock"))
		if lockErr != nil {
			close(hold)
			return
		}
		close(hold)
		<-release
		unlock()
	}()
	select {
	case <-hold:
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle lock was not acquired")
	}
	if _, err := tryLockRuntime(filepath.Join(runtime.InstanceDir(), "lifecycle.lock")); err == nil {
		t.Fatal("stop could enter the lifecycle lock while recover still held it")
	} else if strings.Contains(err.Error(), "仅支持 Linux") {
		close(release)
		t.Skip("lifecycle locks require Linux")
	}
	stopped := make(chan error, 1)
	go func() {
		unlock, lockErr := lockRuntime(context.Background(), filepath.Join(runtime.InstanceDir(), "lifecycle.lock"))
		if lockErr != nil {
			stopped <- lockErr
			return
		}
		defer unlock()
		_, err := runtime.advanceIntent(intentStopped, "")
		stopped <- err
	}()
	select {
	case err := <-stopped:
		close(release)
		t.Fatalf("stop did not wait for the lifecycle lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not finish after the lock was released")
	}
	if runtime.runningGeneration(snapshot) {
		t.Fatal("late recover commit matched a stopped generation")
	}
}

func TestOwnedNetworkRecordIsNotRemovedByAnOlderRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "network.json")
	current := networkRecord{ContainerID: "new", Identity: "id", PID: 20, Started: "boot:20", Generation: 3, RunID: "run-new", SSHAddress: "127.0.0.1:2000", BrowserAddress: "127.0.0.1:2001", VNCAddress: "127.0.0.1:2002", Shared: false, ShareStatus: shareStatusRecovering, NetworkStatus: networkStatusReady}
	if err := writePrivateJSON(path, current); err != nil {
		t.Fatal(err)
	}
	removeOwnedRecord(path, networkRecord{ContainerID: "old", Identity: "id", PID: 10, Started: "boot:10", Generation: 2, RunID: "run-old"})
	if _, err := os.Stat(path); err != nil {
		t.Fatal("older run removed the current network record", err)
	}
	removeOwnedRecord(path, current)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("current run did not remove its own record", err)
	}
	running := filepath.Join(t.TempDir(), "cage-running.json")
	if err := os.WriteFile(running, []byte(`{"pid":8,"started":"boot:8"}`), 0600); err != nil {
		t.Fatal(err)
	}
	removeOwnedRunning(running, 7, "boot:7")
	if _, err := os.Stat(running); err != nil {
		t.Fatal("stale owner removed the live running record", err)
	}
	removeOwnedRunning(running, 8, "boot:8")
	if _, err := os.Stat(running); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestRecoverDoesNotGuessOrRestartWithoutARunningIntent(t *testing.T) {
	for _, name := range []string{"missing", "stopped", "backoff", "config"} {
		t.Run(name, func(t *testing.T) {
			profile := t.TempDir()
			log := filepath.Join(profile, "engine.log")
			script := writeEngineScript(t, profile)
			config := fixtureConfig()
			config.Executable = script
			runtime, err := Configure(profile, config)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("CAGE_ENGINE_LOG", log)
			t.Setenv("CAGE_ENGINE_INFO", `["name=rootless"]`)
			t.Setenv("CAGE_ENGINE_CGROUP", "systemd")
			t.Setenv("CAGE_ENGINE_VERSION", "2")
			cfgPath := filepath.Join(profile, "config.json")
			data, err := json.Marshal(cage.DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfgPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "stopped":
				if _, err := runtime.advanceIntent(intentStopped, "fingerprint"); err != nil {
					t.Fatal(err)
				}
			case "backoff":
				intent, err := runtime.advanceIntent(intentRunning, "fingerprint")
				if err != nil {
					t.Fatal(err)
				}
				intent.NextRecovery = time.Now().Add(time.Minute)
				if err := writePrivateJSON(runtime.intentPath(), intent); err != nil {
					t.Fatal(err)
				}
			case "config":
				if _, err := runtime.advanceIntent(intentRunning, "not-the-current-fingerprint"); err != nil {
					t.Fatal(err)
				}
			}
			restarted, err := runtime.Recover(context.Background(), cfgPath, nil)
			if restarted {
				t.Fatal("recover launched a worker")
			}
			if name == "config" {
				if err == nil || !strings.Contains(err.Error(), "配置") {
					t.Fatalf("config mismatch was not blocked: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(log); err == nil {
				body, _ := os.ReadFile(log)
				t.Fatalf("engine was invoked: %s", body)
			}
		})
	}
}

func TestRecoverBlocksRootlessMismatchAndDoesNotStartStoppedContainer(t *testing.T) {
	for _, name := range []string{"rootless", "stopped-container"} {
		t.Run(name, func(t *testing.T) {
			profile := t.TempDir()
			log := filepath.Join(profile, "engine.log")
			script := writeEngineScript(t, profile)
			config := fixtureConfig()
			config.Executable = script
			runtime, err := Configure(profile, config)
			if err != nil {
				t.Fatal(err)
			}
			info := `["name=rootless"]`
			if name == "rootless" {
				info = `["name=selinux"]`
			}
			t.Setenv("CAGE_ENGINE_LOG", log)
			t.Setenv("CAGE_ENGINE_INFO", info)
			t.Setenv("CAGE_ENGINE_CGROUP", "systemd")
			t.Setenv("CAGE_ENGINE_VERSION", "2")
			t.Setenv("CAGE_ENGINE_LS", "")
			cfgPath := filepath.Join(profile, "config.json")
			data, _ := json.Marshal(cage.DefaultConfig())
			if err := os.WriteFile(cfgPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := runtime.advanceIntent(intentRunning, ""); err != nil {
				t.Fatal(err)
			}
			restarted, err := runtime.Recover(context.Background(), cfgPath, nil)
			if restarted {
				t.Fatal("recover started a worker")
			}
			body, readErr := os.ReadFile(log)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if strings.Contains(string(body), "START") || strings.Contains(string(body), " start ") {
				t.Fatalf("container start was issued: %s", body)
			}
			if name == "rootless" {
				if err == nil || !strings.Contains(err.Error(), "rootless") {
					t.Fatalf("rootless mismatch was not blocked: %v", err)
				}
			} else if err != nil {
				t.Fatalf("stopped container returned %v\n%s", err, body)
			}
		})
	}
}

func writeEngineScript(t *testing.T, profile string) string {
	t.Helper()
	path := filepath.Join(profile, "engine")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$CAGE_ENGINE_LOG"
for arg in "$@"; do
  if [ "$arg" = "start" ]; then
    echo START >> "$CAGE_ENGINE_LOG"
    exit 3
  fi
done
case "$*" in
*info*)
  printf '%s\n' "$CAGE_ENGINE_INFO" "$CAGE_ENGINE_CGROUP" "$CAGE_ENGINE_VERSION"
  ;;
*container*ls*)
  printf '%s' "$CAGE_ENGINE_LS"
  ;;
*)
  echo "unexpected $*" >> "$CAGE_ENGINE_LOG"
  exit 4
  ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}
