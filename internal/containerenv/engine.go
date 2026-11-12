package containerenv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type State struct {
	Exists            bool   `json:"exists"`
	Running           bool   `json:"running"`
	Ready             bool   `json:"ready"`
	ContainerID       string `json:"container_id,omitempty"`
	PID               int    `json:"pid,omitempty"`
	MemoryBytes       int64  `json:"memory_bytes"`
	CPUs              int    `json:"cpus"`
	BrowserAddress    string `json:"browser_address,omitempty"`
	VNCAddress        string `json:"vnc_address,omitempty"`
	StoragePath       string `json:"storage_path,omitempty"`
	ResourcesVerified bool   `json:"resources_verified"`
	ResourceError     string `json:"resource_error,omitempty"`
	PIDs              int64  `json:"pids"`
	MountsReady       bool   `json:"mounts_ready"`
	MountError        string `json:"mount_error,omitempty"`
	NetworkReady      bool   `json:"network_ready"`
	Shared            bool   `json:"shared"`
	ShareStatus       string `json:"share_status,omitempty"`
	NetworkStatus     string `json:"network_status,omitempty"`
	Intent            string `json:"intent,omitempty"`
	IntentGeneration  uint64 `json:"intent_generation,omitempty"`
}

type Inspection struct {
	State              State            `json:"state"`
	Rootless           bool             `json:"rootless"`
	NetworkConfined    bool             `json:"network_confined"`
	FilesystemConfined bool             `json:"filesystem_confined"`
	Evidence           string           `json:"evidence"`
	MountPropagation   MountPropagation `json:"mount_propagation"`
}

type engineContainer struct {
	ID    string `json:"Id"`
	Image string `json:"Image"`
	State struct {
		Running bool
		Pid     int
	}
	Config struct {
		Labels map[string]string
		Image  string
	}
	HostConfig struct {
		Privileged   bool
		NetworkMode  string
		PidMode      string
		IpcMode      string
		CgroupnsMode string
		CgroupParent string
		UsernsMode   string
		Binds        []string
		ExtraHosts   []string
		CapAdd       []string
		Memory       int64
		NanoCpus     int64
		Devices      []struct {
			PathOnHost      string
			PathInContainer string
		}
	}
	Mounts []struct {
		Type        string
		Source      string
		Destination string
	}
}

func (r *Runtime) command(ctx context.Context, args ...string) *exec.Cmd {
	if r.config.Engine == "docker" {
		// Docker's normal client config can inject the user's proxy URLs and
		// credentials into new containers. Use a profile-private empty config.
		args = append([]string{"--config", filepath.Join(r.InstanceDir(), "client-config"), "--host", r.config.Socket}, args...)
	}
	command := exec.CommandContext(ctx, r.config.Executable, args...)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "CONTAINER_HOST", "CONTAINER_CONNECTION":
			continue
		}
		command.Env = append(command.Env, entry)
	}
	return command
}

func (r *Runtime) output(ctx context.Context, args ...string) ([]byte, error) {
	command := r.command(ctx, args...)
	var output, diagnostic bytes.Buffer
	command.Stdout = &limitedWriter{writer: &output, left: 1 << 20}
	command.Stderr = &limitedWriter{writer: &diagnostic, left: 4096}
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("容器引擎操作失败: %w: %s", err, strings.TrimSpace(diagnostic.String()))
	}
	return output.Bytes(), nil
}

type limitedWriter struct {
	writer *bytes.Buffer
	left   int
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if len(data) > w.left {
		return 0, errors.New("container engine output limit exceeded")
	}
	n, err := w.writer.Write(data)
	w.left -= n
	return n, err
}

func (r *Runtime) inspectContainer(ctx context.Context) (*engineContainer, error) {
	// Query only this exact managed name. An empty result is absence, while
	// engine/permission failures remain errors rather than triggering creation.
	data, err := r.output(ctx, "container", "ls", "--all", "--filter", "label="+profileLabel+"="+r.identity, "--format", "{{.ID}}")
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return nil, nil
	}
	if strings.ContainsAny(id, " \r\n\t") {
		return nil, errors.New("容器标识不唯一")
	}
	data, err = r.output(ctx, "container", "inspect", id)
	if err != nil {
		return nil, err
	}
	var found []engineContainer
	if json.Unmarshal(data, &found) != nil || len(found) != 1 || found[0].Config.Labels[profileLabel] != r.identity {
		return nil, errors.New("容器身份与当前 Cage 配置不匹配")
	}
	return &found[0], nil
}

func (r *Runtime) State(ctx context.Context) (State, error) {
	container, err := r.inspectContainer(ctx)
	if err != nil {
		return State{}, err
	}
	if container == nil {
		storage, err := r.StoragePath(ctx)
		state := State{CPUs: r.config.CPUs, MemoryBytes: r.config.MemoryBytes, StoragePath: storage}
		r.annotateIntent(&state)
		return state, err
	}
	state := State{Exists: true, Running: container.State.Running, ContainerID: container.ID, PID: container.State.Pid, MemoryBytes: container.HostConfig.Memory, CPUs: int(container.HostConfig.NanoCpus / 1e9)}
	state.StoragePath, err = r.StoragePath(ctx)
	if err != nil {
		return State{}, err
	}
	if state.Running {
		mounts, mountErr := r.inspectMounts(container)
		if mountErr != nil {
			state.MountError = mountErr.Error()
		} else {
			state.MountsReady = mounts.Ready()
		}
		resources, resourceErr := r.verifyResources(ctx, container.ID)
		if resourceErr != nil {
			state.MemoryBytes = 0
			state.CPUs = 0
			state.ResourceError = resourceErr.Error()
		} else {
			state.MemoryBytes = resources.MemoryBytes
			state.CPUs = resources.CPUs
			state.PIDs = resources.PIDs
			state.ResourcesVerified = true
		}
		if record, err := r.networkState(ctx, container.ID); err == nil {
			applyNetworkRecord(&state, record)
		}
	}
	r.annotateIntent(&state)
	return state, nil
}

// StoragePath reports the engine's actual persistent storage location. An
// explicitly supplied rootless engine may already store data outside the
// profile; this path is never treated as disposable or removed by Cage.
func (r *Runtime) StoragePath(ctx context.Context) (string, error) {
	format := "{{.DockerRootDir}}"
	if r.config.Engine == "podman" {
		format = "{{.Store.GraphRoot}}"
	}
	data, err := r.output(ctx, "info", "--format", format)
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(data))
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return "", errors.New("容器持久存储位置无效")
	}
	return path, nil
}

func (r *Runtime) rootlessEngine(ctx context.Context) (bool, error) {
	rootless, limited, err := r.engineSecurity(ctx)
	if err != nil {
		return false, err
	}
	if !limited {
		return false, errors.New("容器引擎必须使用 systemd 和 cgroup v2，以执行 CPU、内存与进程数限额")
	}
	return rootless, nil
}

// Shutdown still works if an engine loses its resource-controller support.
// Starting and inspecting enforce the stronger rootlessEngine precondition.
func (r *Runtime) engineSecurity(ctx context.Context) (rootless, limited bool, err error) {
	if r.config.Engine == "docker" {
		data, err := r.output(ctx, "info", "--format", "{{json .SecurityOptions}}\n{{.CgroupDriver}}\n{{.CgroupVersion}}")
		if err != nil {
			return false, false, err
		}
		var options []string
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) != 3 || json.Unmarshal([]byte(lines[0]), &options) != nil {
			return false, false, errors.New("无法确认 Docker 安全配置")
		}
		limited = lines[1] == "systemd" && lines[2] == "2"
		for _, option := range options {
			if option == "name=rootless" || strings.HasPrefix(option, "name=rootless,") {
				return true, limited, nil
			}
		}
		return false, limited, nil
	}
	data, err := r.output(ctx, "info", "--format", "{{.Host.Security.Rootless}}\n{{.Host.CgroupManager}}\n{{.Host.CgroupVersion}}")
	if err != nil {
		return false, false, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		return false, false, errors.New("无法确认 Podman 安全配置")
	}
	return lines[0] == "true", lines[1] == "systemd" && (lines[2] == "v2" || lines[2] == "2"), nil
}

func checkContainerConfiguration(container *engineContainer) error {
	config := container.HostConfig
	if config.Privileged || config.NetworkMode != "none" || (config.PidMode != "" && config.PidMode != "private") || config.IpcMode != "private" || config.CgroupnsMode != "private" {
		return errors.New("容器必须使用独立命名空间和禁用默认网络，不能启用特权模式")
	}
	if len(config.Binds) > 0 {
		return errors.New("容器不能直接挂载宿主目录")
	}
	for _, entry := range config.ExtraHosts {
		if entry != "cage:127.0.1.1" {
			return errors.New("容器只允许 Cage 主机名的固定回环地址")
		}
	}
	for _, mount := range container.Mounts {
		if mount.Type == "bind" || mount.Type == "volume" {
			return errors.New("容器存在未经 Cage 文件共享授权的挂载")
		}
	}
	for _, device := range config.Devices {
		if (device.PathOnHost != "/dev/fuse" && device.PathOnHost != "/dev/net/tun") || device.PathOnHost != device.PathInContainer {
			return errors.New("容器存在额外宿主设备")
		}
	}
	for _, capability := range config.CapAdd {
		capability = strings.TrimPrefix(capability, "CAP_")
		if capability != "NET_ADMIN" && capability != "SYS_ADMIN" {
			return errors.New("容器存在额外系统权限")
		}
	}
	return nil
}

func (r *Runtime) Inspect(ctx context.Context) (Inspection, error) {
	rootless, err := r.rootlessEngine(ctx)
	if err != nil {
		return Inspection{}, err
	}
	if !rootless {
		return Inspection{}, errors.New("Cage 容器必须由独立 rootless 引擎运行")
	}
	container, err := r.inspectContainer(ctx)
	if err != nil {
		return Inspection{}, err
	}
	if container == nil {
		return Inspection{Rootless: true}, nil
	}
	if err := checkContainerConfiguration(container); err != nil {
		return Inspection{}, err
	}
	if container.HostConfig.CgroupParent != r.resourceSlice() {
		return Inspection{}, errors.New("已有容器缺少当前配置的宿主资源边界；保留原数据，拒绝无约束启动")
	}
	state := State{Exists: true, Running: container.State.Running, ContainerID: container.ID, PID: container.State.Pid, MemoryBytes: container.HostConfig.Memory, CPUs: int(container.HostConfig.NanoCpus / 1e9)}
	state.StoragePath, err = r.StoragePath(ctx)
	if err != nil {
		return Inspection{}, err
	}
	result := Inspection{State: state, Rootless: true, Evidence: "rootless 引擎；无默认网络或宿主目录挂载；等待运行中的网络与文件共享检查"}
	if state.Running {
		mounts, err := r.inspectMounts(container)
		if err != nil {
			return Inspection{}, err
		}
		result.MountPropagation = mounts
		result.State.MountsReady = mounts.Ready()
		resources, err := r.verifyResources(ctx, container.ID)
		if err != nil {
			return Inspection{}, err
		}
		result.State.MemoryBytes = resources.MemoryBytes
		result.State.CPUs = resources.CPUs
		result.State.PIDs = resources.PIDs
		result.State.ResourcesVerified = true
		if record, err := r.networkState(ctx, container.ID); err == nil {
			applyNetworkRecord(&result.State, record)
			result.NetworkConfined = true
			result.FilesystemConfined = mounts.Ready()
			if result.State.Shared {
				result.Evidence = "实际 rootless 容器无默认网络或宿主目录挂载；当前外置网络门槛身份已验证，唯一 TAP 通道由宿主接收；默认项目经受限 SFTP 共享并完成双端读写验证"
			} else {
				result.Evidence = "实际 rootless 容器无默认网络或宿主目录挂载；当前外置网络门槛身份已验证，唯一 TAP 通道由宿主接收；默认项目共享尚未就绪"
			}
		}
	}
	r.annotateIntent(&result.State)
	return result, nil
}
