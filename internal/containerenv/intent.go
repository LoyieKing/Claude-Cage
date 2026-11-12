package containerenv

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/sylvinus/agent-vm/internal/host"
)

const (
	intentRunning          = "running"
	intentStopped          = "stopped"
	shareStatusReady       = "ready"
	shareStatusRecovering  = "recovering"
	shareStatusDegraded    = "degraded"
	shareStatusBlocked     = "blocked"
	networkStatusReady     = "ready"
	runtimeGenerationEnv   = "CAGE_RUNTIME_GENERATION"
	runtimeIntentVersion   = 1
	recoveryBackoffInitial = time.Second
	recoveryBackoffMax     = 30 * time.Second
)

var errRecoverySuperseded = errors.New("容器停止请求优先于网络恢复")

// runtimeIntent is the host-persisted desired lifecycle. Generation changes on
// every explicit transition so a Recover that observed an older running
// generation cannot publish a worker after Stop.
type runtimeIntent struct {
	Version          int       `json:"version"`
	Desired          string    `json:"desired"`
	Generation       uint64    `json:"generation"`
	Fingerprint      string    `json:"fingerprint,omitempty"`
	RecoveryAttempts int       `json:"recovery_attempts,omitempty"`
	NextRecovery     time.Time `json:"next_recovery,omitempty"`
}

type networkRecord struct {
	ContainerID    string `json:"container_id"`
	Identity       string `json:"identity,omitempty"`
	PID            int    `json:"pid"`
	Started        string `json:"started,omitempty"`
	Generation     uint64 `json:"generation,omitempty"`
	RunID          string `json:"run_id"`
	SSHAddress     string `json:"ssh_address"`
	BrowserAddress string `json:"browser_address"`
	VNCAddress     string `json:"vnc_address"`
	Shared         bool   `json:"shared"`
	ShareStatus    string `json:"share_status,omitempty"`
	NetworkStatus  string `json:"network_status,omitempty"`
}

type recoveryFacts struct {
	Desired            string
	SnapshotGeneration uint64
	CurrentGeneration  uint64
	ContainerExists    bool
	ContainerRunning   bool
	MountsReady        bool
	Rootless           bool
	ResourcesVerified  bool
	ConfigMatches      bool
	WorkerAlive        bool
	NetworkLockHeld    bool
	BackingOff         bool
}

func (r *Runtime) intentPath() string {
	return filepath.Join(r.InstanceDir(), "runtime-intent.json")
}

func (r *Runtime) networkRecordPath() string {
	return filepath.Join(r.InstanceDir(), "network.json")
}

func (r *Runtime) readIntent() (runtimeIntent, error) {
	path := r.intentPath()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return runtimeIntent{Version: runtimeIntentVersion, Desired: intentStopped}, nil
	}
	if err != nil {
		return runtimeIntent{}, err
	}
	if err := host.CheckPrivateFile(path); err != nil {
		return runtimeIntent{}, err
	}
	if len(data) > 8192 {
		return runtimeIntent{}, errors.New("容器运行意图记录无效")
	}
	var intent runtimeIntent
	if json.Unmarshal(data, &intent) != nil || intent.Version != runtimeIntentVersion || (intent.Desired != intentRunning && intent.Desired != intentStopped) {
		return runtimeIntent{}, errors.New("容器运行意图记录无效")
	}
	return intent, nil
}

func (r *Runtime) advanceIntent(desired, fingerprint string) (runtimeIntent, error) {
	current, err := r.readIntent()
	if err != nil {
		return runtimeIntent{}, err
	}
	current.Version = runtimeIntentVersion
	current.Generation++
	current.Desired = desired
	if fingerprint != "" {
		current.Fingerprint = fingerprint
	}
	if desired == intentRunning {
		current.RecoveryAttempts = 0
		current.NextRecovery = time.Time{}
	}
	if err := writePrivateJSON(r.intentPath(), current); err != nil {
		return runtimeIntent{}, err
	}
	return current, nil
}

// noteRunningIntent records an explicit running desire without bumping the
// generation of a worker that is already alive. A stopped intent is left
// unchanged so Stop keeps precedence.
func (r *Runtime) noteRunningIntent(fingerprint string) error {
	current, err := r.readIntent()
	if err != nil {
		return err
	}
	if current.Desired == intentStopped {
		if current.Generation > 0 {
			return errors.New("容器正在停止，请稍后重新启动")
		}
	}
	if current.Desired == intentRunning && current.Generation > 0 && current.Fingerprint == fingerprint {
		return r.clearRecoveryBackoff(current)
	}
	current.Version = runtimeIntentVersion
	current.Desired = intentRunning
	if current.Generation == 0 {
		current.Generation = 1
	}
	current.Fingerprint = fingerprint
	current.RecoveryAttempts = 0
	current.NextRecovery = time.Time{}
	return writePrivateJSON(r.intentPath(), current)
}

func (r *Runtime) runningGeneration(snapshot runtimeIntent) bool {
	current, err := r.readIntent()
	if err != nil {
		return false
	}
	return current.Desired == intentRunning && current.Generation == snapshot.Generation
}

func (r *Runtime) clearRecoveryBackoff(snapshot runtimeIntent) error {
	current, err := r.readIntent()
	if err != nil {
		return err
	}
	if current.Desired != intentRunning || current.Generation != snapshot.Generation {
		return nil
	}
	if current.RecoveryAttempts == 0 && current.NextRecovery.IsZero() {
		return nil
	}
	current.RecoveryAttempts = 0
	current.NextRecovery = time.Time{}
	return writePrivateJSON(r.intentPath(), current)
}

func (r *Runtime) bumpRecoveryBackoff(snapshot runtimeIntent) error {
	current, err := r.readIntent()
	if err != nil {
		return err
	}
	if current.Desired != intentRunning || current.Generation != snapshot.Generation {
		return nil
	}
	current.RecoveryAttempts++
	delay := recoveryBackoffInitial
	for attempt := 1; attempt < current.RecoveryAttempts && delay < recoveryBackoffMax; attempt++ {
		delay *= 2
	}
	if delay > recoveryBackoffMax {
		delay = recoveryBackoffMax
	}
	current.NextRecovery = time.Now().Add(delay)
	return writePrivateJSON(r.intentPath(), current)
}

func intentBackoffActive(intent runtimeIntent, now time.Time) bool {
	return !intent.NextRecovery.IsZero() && now.Before(intent.NextRecovery)
}

// planNetworkRecovery decides whether a dead network worker may be launched.
// It never selects a stopped container, a live worker, or a mismatched
// rootless/resource/config identity. Share health is not an input: a live
// worker reconnects its own default share.
func planNetworkRecovery(facts recoveryFacts) (bool, error) {
	if facts.Desired != intentRunning || facts.CurrentGeneration != facts.SnapshotGeneration {
		return false, nil
	}
	if facts.BackingOff || facts.NetworkLockHeld || facts.WorkerAlive {
		return false, nil
	}
	if !facts.ConfigMatches {
		return false, errors.New("容器运行配置已变化，请先停止环境后重新启动")
	}
	if !facts.Rootless {
		return false, errors.New("拒绝操作非 rootless 容器引擎")
	}
	if !facts.ContainerExists || !facts.ContainerRunning {
		return false, nil
	}
	if !facts.ResourcesVerified {
		return false, errors.New("容器资源边界未通过核验，拒绝恢复网络")
	}
	if !facts.MountsReady {
		return false, errors.New("容器的项目挂载尚未初始化，请先完整停止环境再启动；文件和登录状态会保留")
	}
	return true, nil
}

func shareReady(record networkRecord) bool {
	if record.ShareStatus == "" {
		return record.Shared
	}
	return record.Shared && record.ShareStatus == shareStatusReady
}

func applyNetworkRecord(state *State, record networkRecord) {
	state.NetworkReady = true
	state.NetworkStatus = record.NetworkStatus
	if state.NetworkStatus == "" {
		state.NetworkStatus = networkStatusReady
	}
	state.ShareStatus = record.ShareStatus
	state.Shared = shareReady(record)
	state.BrowserAddress = record.BrowserAddress
	state.VNCAddress = record.VNCAddress
	state.Ready = state.ResourcesVerified && state.MountsReady && state.NetworkReady
}

func (r *Runtime) annotateIntent(state *State) {
	intent, err := r.readIntent()
	if err != nil {
		return
	}
	state.Intent = intent.Desired
	state.IntentGeneration = intent.Generation
}

func generationAllowed(intent runtimeIntent, envGeneration string) error {
	if intent.Desired != intentRunning {
		return errors.New("容器网络运行意图已停止")
	}
	if envGeneration != "" && envGeneration != strconv.FormatUint(intent.Generation, 10) {
		return errors.New("容器网络运行代次已变化")
	}
	return nil
}

func removeOwnedRecord(path string, owner networkRecord) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 8192 {
		return
	}
	var current networkRecord
	if json.Unmarshal(data, &current) != nil {
		return
	}
	if current.PID != owner.PID || current.Started != owner.Started || current.Generation != owner.Generation || current.RunID != owner.RunID || current.Identity != owner.Identity {
		return
	}
	_ = os.Remove(path)
}

func removeOwnedRunning(path string, pid int, started string) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 8192 {
		return
	}
	var current struct {
		PID     int    `json:"pid"`
		Started string `json:"started"`
	}
	if json.Unmarshal(data, &current) != nil || current.PID != pid || current.Started != started {
		return
	}
	_ = os.Remove(path)
}
