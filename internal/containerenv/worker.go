package containerenv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/host"
)

const guestHelper = "/usr/local/lib/cage-runtime"

func (r *Runtime) readNetworkRecord(containerID string) (networkRecord, error) {
	path := r.networkRecordPath()
	if err := host.CheckPrivateFile(path); err != nil {
		return networkRecord{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 8192 {
		return networkRecord{}, errors.New("容器连接记录不可用")
	}
	var record networkRecord
	if json.Unmarshal(data, &record) != nil || record.ContainerID != containerID || record.RunID == "" {
		return record, errors.New("容器连接记录不属于当前运行环境")
	}
	if record.Identity != "" && record.Identity != r.identity {
		return record, errors.New("容器连接记录不属于当前运行环境")
	}
	for _, address := range []string{record.SSHAddress, record.BrowserAddress, record.VNCAddress} {
		hostName, port, splitErr := net.SplitHostPort(address)
		number, parseErr := strconv.Atoi(port)
		ip := net.ParseIP(hostName)
		if splitErr != nil || parseErr != nil || ip == nil || !ip.IsLoopback() || number < 1024 || number > 65535 {
			return record, errors.New("容器连接入口无效")
		}
	}
	if !processAlive(record.PID, record.Started) {
		return record, errors.New("容器网络服务未运行")
	}
	return record, nil
}

func (r *Runtime) networkState(ctx context.Context, containerID string) (networkRecord, error) {
	record, err := r.readNetworkRecord(containerID)
	if err != nil {
		return record, err
	}
	client, err := cage.OpenGateClient(r.InstanceDir())
	if err != nil {
		return record, err
	}
	status, err := client.Status(ctx)
	if err != nil || status.RunID != record.RunID {
		return record, errors.New("容器网络控制服务尚未连接")
	}
	return record, nil
}

// Start starts an existing prepared container and its host network worker.
// Parent cancellation before readiness terminates only this newly created
// worker; the persistent container and all of its files remain available.
func (r *Runtime) Start(ctx context.Context, configPath string, log io.Writer) error {
	cfg, err := cage.Load(configPath)
	if err != nil {
		return err
	}
	if err := cfg.RequireManagedProxy(r.profile); err != nil {
		return err
	}
	if err := host.EnsurePrivateDir(r.InstanceDir()); err != nil {
		return err
	}
	unlock, err := lockRuntime(ctx, filepath.Join(r.InstanceDir(), "lifecycle.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if err := r.ensureResourceSlice(ctx); err != nil {
		return err
	}
	inspection, err := r.Inspect(ctx)
	if err != nil {
		return err
	}
	if !inspection.State.Exists {
		return errors.New("容器尚未准备，请先运行 Cage 容器安装")
	}
	if inspection.State.Running {
		if !inspection.State.MountsReady {
			return errors.New("容器的项目挂载尚未初始化，请先完整停止环境再启动；文件和登录状态会保留")
		}
		if record, err := r.networkState(ctx, inspection.State.ContainerID); err == nil {
			client, err := cage.OpenGateClient(r.InstanceDir())
			if err != nil {
				return err
			}
			state, err := client.Status(ctx)
			if err != nil {
				return err
			}
			if state.Configuration != cfg.Fingerprint() {
				return errors.New("容器运行配置已变化，请先停止环境后重新启动")
			}
			if err := r.noteRunningIntent(cfg.Fingerprint()); err != nil {
				return err
			}
			reportNetworkReady(log, record)
			return nil
		}
	} else if _, err := r.output(ctx, "start", inspection.State.ContainerID); err != nil {
		return err
	}
	if _, err := r.verifyResources(ctx, inspection.State.ContainerID); err != nil {
		if !inspection.State.Running {
			r.stopContainerNow(inspection.State.ContainerID)
		}
		return err
	}
	if !inspection.State.Running {
		if err := r.initializeMounts(ctx, inspection.State.ContainerID); err != nil {
			r.stopContainerNow(inspection.State.ContainerID)
			return err
		}
	}
	if err := r.InstallAccess(ctx); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if _, err := r.output(ctx, "cp", executable, inspection.State.ContainerID+":"+guestHelper); err != nil {
		return err
	}
	if _, err := r.output(ctx, "exec", "--user", "0", inspection.State.ContainerID, "chmod", "0755", guestHelper); err != nil {
		return err
	}
	intent, err := r.advanceIntent(intentRunning, cfg.Fingerprint())
	if err != nil {
		return err
	}
	if !r.runningGeneration(intent) {
		return errRecoverySuperseded
	}
	if err := r.waitNetworkUnlocked(ctx); err != nil {
		return err
	}
	command, done, err := r.startNetworkProcess(configPath, intent.Generation)
	if err != nil {
		return err
	}
	record, err := r.waitForNetwork(ctx, command, done, inspection.State.ContainerID, intent.Generation, 45*time.Second)
	if err != nil {
		return err
	}
	_ = r.clearRecoveryBackoff(intent)
	reportNetworkReady(log, record)
	return nil
}

// Recover restarts only the host network worker. The container must already
// be running and the persisted desired intent must already be running. A
// missing intent is not treated as running: legacy instances wait for an
// explicit Start. Recover never starts a stopped container and never changes
// pause, proxy, or configuration.
//
// restarted is true only after a new worker has published a fresh closed
// egress gate. The caller drops guest control sessions, clears cached safety,
// reads that gate, and schedules the existing safety check only when the
// pause generation is unchanged. State.NetworkReady is the worker liveness
// bit; State.Shared is the default workspace and must not by itself cause
// another Recover.
func (r *Runtime) Recover(ctx context.Context, configPath string, log io.Writer) (bool, error) {
	if err := host.EnsurePrivateDir(r.InstanceDir()); err != nil {
		return false, err
	}
	unlock, err := lockRuntime(ctx, filepath.Join(r.InstanceDir(), "lifecycle.lock"))
	if err != nil {
		return false, err
	}
	defer unlock()
	intent, err := r.readIntent()
	if err != nil {
		return false, err
	}
	// No intent file is desired=stopped at generation 0. Do not guess.
	if intent.Desired != intentRunning {
		return false, nil
	}
	if intentBackoffActive(intent, time.Now()) {
		return false, nil
	}
	held, err := r.networkLockHeld()
	if err != nil {
		return false, err
	}
	if held {
		return false, nil
	}
	cfg, err := cage.Load(configPath)
	if err != nil {
		return false, err
	}
	if intent.Fingerprint != "" && intent.Fingerprint != cfg.Fingerprint() {
		return false, errors.New("容器运行配置已变化，请先停止环境后重新启动")
	}
	inspection, err := r.Inspect(ctx)
	if err != nil {
		return false, err
	}
	workerAlive := false
	if inspection.State.Running && inspection.State.ContainerID != "" {
		if _, stateErr := r.networkState(ctx, inspection.State.ContainerID); stateErr == nil {
			workerAlive = true
		}
	}
	restart, err := planNetworkRecovery(recoveryFacts{
		Desired:            intent.Desired,
		SnapshotGeneration: intent.Generation,
		CurrentGeneration:  intent.Generation,
		ContainerExists:    inspection.State.Exists,
		ContainerRunning:   inspection.State.Running,
		MountsReady:        inspection.State.MountsReady,
		Rootless:           inspection.Rootless,
		ResourcesVerified:  inspection.State.ResourcesVerified,
		ConfigMatches:      true,
		WorkerAlive:        workerAlive,
	})
	if err != nil || !restart {
		return false, err
	}
	if err := cfg.RequireManagedProxy(r.profile); err != nil {
		return false, err
	}
	if !r.runningGeneration(intent) {
		return false, nil
	}
	if err := r.waitNetworkUnlocked(ctx); err != nil {
		return false, err
	}
	if !r.runningGeneration(intent) {
		return false, nil
	}
	command, done, err := r.startNetworkProcess(configPath, intent.Generation)
	if err != nil {
		_ = r.bumpRecoveryBackoff(intent)
		return false, err
	}
	record, err := r.waitForNetwork(ctx, command, done, inspection.State.ContainerID, intent.Generation, 20*time.Second)
	if errors.Is(err, errRecoverySuperseded) {
		return false, nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return false, err
		}
		_ = r.bumpRecoveryBackoff(intent)
		return false, err
	}
	_ = r.clearRecoveryBackoff(intent)
	reportNetworkReady(log, record)
	return true, nil
}

// ServeNetwork runs in the dedicated `cage container-network` process. TAP
// failure still closes the gate. Default-share EOF and browser or VNC dial
// failures stay inside this process and reconnect without a new gate.
func (r *Runtime) ServeNetwork(ctx context.Context, configPath string) error {
	if err := host.EnsurePrivateDir(r.InstanceDir()); err != nil {
		return err
	}
	unlock, err := tryLockRuntime(filepath.Join(r.InstanceDir(), "network.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopWatch, err := cage.WatchJobStop(r.profile, os.Getenv(cage.JobEnv), cancel)
	if err != nil {
		return err
	}
	defer stopWatch()
	intent, err := r.readIntent()
	if err != nil {
		return err
	}
	if err := generationAllowed(intent, os.Getenv(runtimeGenerationEnv)); err != nil {
		return err
	}
	generation := intent.Generation
	go r.watchRuntimeIntent(ctx, generation, cancel)
	inspection, err := r.Inspect(ctx)
	if err != nil {
		return err
	}
	if !inspection.State.Running {
		return errors.New("容器未运行")
	}
	if !inspection.State.MountsReady {
		return errors.New("容器项目挂载尚未初始化，请完整停止后重新启动")
	}
	cfg, err := cage.Load(configPath)
	if err != nil {
		return err
	}
	if intent.Fingerprint != "" && intent.Fingerprint != cfg.Fingerprint() {
		return errors.New("容器运行配置已变化，请先停止环境后重新启动")
	}
	if err := cfg.RequireManagedProxy(r.profile); err != nil {
		return err
	}
	network, err := NewNetwork(cfg)
	if err != nil {
		return err
	}
	defer network.Gate.Close("容器网络服务已停止")
	command := r.command(ctx, "exec", "-i", "--user", "0", inspection.State.ContainerID, guestHelper, "container-tap")
	command.Stderr = os.Stderr
	endpoint, wait, err := ProcessEndpoint(command)
	if err != nil {
		return err
	}
	defer func() { endpoint.Close(); _ = wait() }()
	networkDone := make(chan error, 1)
	go func() {
		err := network.Run(ctx, endpoint)
		cancel()
		networkDone <- err
	}()
	sshListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer sshListener.Close()
	browserListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer browserListener.Close()
	vncListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer vncListener.Close()
	if err := r.writeSSHConfig(sshListener.Addr().(*net.TCPAddr).Port); err != nil {
		return err
	}
	sshDone := make(chan error, 1)
	go func() { sshDone <- network.ServeSSH(ctx, sshListener) }()
	var services sync.WaitGroup
	defer func() {
		cancel()
		services.Wait()
	}()
	services.Add(1)
	go func() {
		defer services.Done()
		r.serveForward(ctx, network, browserListener, "127.0.0.1:18500")
	}()
	services.Add(1)
	go func() {
		defer services.Done()
		r.serveForward(ctx, network, vncListener, "127.0.0.1:15900")
	}()
	stopGate, err := cage.StartGateControl(configPath, r.InstanceDir(), network.Gate)
	if err != nil {
		return err
	}
	defer stopGate()
	if err := cfg.RecordRunningAt(r.InstanceDir()); err != nil {
		return err
	}
	started := processStarted(os.Getpid())
	publisher := newRecordPublisher(r.networkRecordPath(), networkRecord{
		ContainerID:    inspection.State.ContainerID,
		Identity:       r.identity,
		PID:            os.Getpid(),
		Started:        started,
		Generation:     generation,
		RunID:          network.Gate.Status().RunID,
		SSHAddress:     sshListener.Addr().String(),
		BrowserAddress: browserListener.Addr().String(),
		VNCAddress:     vncListener.Addr().String(),
		Shared:         false,
		ShareStatus:    shareStatusRecovering,
		NetworkStatus:  networkStatusReady,
	})
	if err := publisher.write(); err != nil {
		return err
	}
	defer func() {
		owner := publisher.owner()
		removeOwnedRecord(r.networkRecordPath(), owner)
		removeOwnedRunning(filepath.Join(r.InstanceDir(), "cage-running.json"), owner.PID, owner.Started)
	}()
	services.Add(1)
	go func() {
		defer services.Done()
		r.superviseGuestServices(ctx, network, publisher)
	}()
	err = waitNetworkExit(ctx, networkDone, sshDone)
	return err
}

func (r *Runtime) stopContainerNow(containerID string) {
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, _ = r.output(stopCtx, "stop", "--time", "20", containerID)
	cancel()
}

func (r *Runtime) waitNetworkUnlocked(ctx context.Context) error {
	wait, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	release, err := lockRuntime(wait, filepath.Join(r.InstanceDir(), "network.lock"))
	if err != nil {
		return errors.New("容器网络服务仍在退出；原文件保留，请稍后重试")
	}
	release()
	return nil
}

func (r *Runtime) startNetworkProcess(configPath string, generation uint64) (*exec.Cmd, <-chan error, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	logFile, err := os.OpenFile(filepath.Join(r.InstanceDir(), "network.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, nil, err
	}
	command := exec.Command(executable, "container-network")
	command.Env = append(os.Environ(), cage.ConfigEnv+"="+configPath, runtimeGenerationEnv+"="+strconv.FormatUint(generation, 10))
	command.Stdout = logFile
	command.Stderr = logFile
	detachWorker(command)
	if err := command.Start(); err != nil {
		logFile.Close()
		return nil, nil, err
	}
	// The child inherited the log fd. Closing the parent copy here avoids a
	// Wait goroutine racing a later launch on the same writer.
	if err := logFile.Close(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, nil, err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	return command, done, nil
}

func (r *Runtime) waitForNetwork(ctx context.Context, command *exec.Cmd, done <-chan error, containerID string, generation uint64, timeout time.Duration) (networkRecord, error) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	fail := func(err error) (networkRecord, error) {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		return networkRecord{}, err
	}
	for {
		select {
		case err := <-done:
			if err == nil {
				err = errors.New("容器网络服务退出")
			}
			return networkRecord{}, fmt.Errorf("容器网络服务退出，请查看容器网络日志: %w", err)
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-timer.C:
			return fail(errors.New("容器连接超时；容器文件保留，请查看网络日志"))
		case <-ticker.C:
			if !r.runningGeneration(runtimeIntent{Desired: intentRunning, Generation: generation}) {
				return fail(errRecoverySuperseded)
			}
			checkCtx, cancel := context.WithTimeout(ctx, time.Second)
			record, err := r.networkState(checkCtx, containerID)
			cancel()
			if err != nil || record.PID != command.Process.Pid || record.Generation != generation {
				continue
			}
			// The child is healthy once its own record matches this process and
			// the fresh gate. Share degradation must not kill it.
			return record, nil
		}
	}
}

func reportNetworkReady(log io.Writer, record networkRecord) {
	if log == nil {
		return
	}
	switch {
	case shareReady(record):
		fmt.Fprintln(log, "Cage container network and protected project share are ready.")
	case record.ShareStatus == shareStatusBlocked:
		fmt.Fprintln(log, "Cage container network is ready. The default project share is blocked because its directory identity changed; the network was left running.")
	default:
		fmt.Fprintln(log, "Cage container network is ready; the protected project share is still reconnecting.")
	}
}

func (r *Runtime) GuestCommand(ctx context.Context, args ...string) *exec.Cmd {
	base := []string{"-F", r.SSHConfig(), "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes", "-o", "ConnectTimeout=3"}
	return exec.CommandContext(ctx, "ssh", append(base, args...)...)
}
