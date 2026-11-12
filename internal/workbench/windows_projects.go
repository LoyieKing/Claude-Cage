package workbench

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lima-vm/sshocker/pkg/reversesshfs"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

type windowsProjectBinding struct{ ID, Grant, Path string }

func windowsProjectIdentity(profile, hostPath string) windowsProjectBinding {
	digest := sha256.Sum256([]byte(filepath.Clean(profile) + "\x00" + filepath.Clean(hostPath)))
	return windowsProjectBinding{ID: fmt.Sprintf("%x", digest[:8]), Grant: randomToken()}
}

type bufferedProjectOutput struct {
	*bufio.Reader
	io.Closer
}

func startWindowsProjectTransport(cmd *exec.Cmd, hostPath string, binding windowsProjectBinding) (*projectShare, error) {
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		output.Close()
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		input.Close()
		output.Close()
		return nil, err
	}
	timeoutDone := make(chan struct{})
	timeout := time.AfterFunc(15*time.Second, func() { defer close(timeoutDone); _ = cmd.Process.Kill(); _ = input.Close() })
	admitted := false
	defer func() {
		if !admitted {
			cancelWindowsAdmissionDeadline(timeout, timeoutDone)
		}
	}()
	data, _ := json.Marshal(map[string]any{"action": "mount", "id": binding.ID, "grant": binding.Grant})
	if _, err = input.Write(append(data, '\n')); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	reader := bufio.NewReaderSize(output, 16384)
	line, err := reader.ReadSlice('\n')
	var started struct {
		Started bool   `json:"started"`
		ID      string `json:"id"`
		Path    string `json:"path"`
	}
	if err != nil || len(line) > 16384 || json.Unmarshal(line, &started) != nil || !started.Started || started.ID != binding.ID || !validWindowsProjectPath(started.Path, binding.ID) {
		_ = cmd.Process.Kill()
		_ = input.Close()
		_ = output.Close()
		_ = cmd.Wait()
		return nil, errors.New("Windows 项目助手尚未准备好挂载")
	}
	if !cancelWindowsAdmissionDeadline(timeout, timeoutDone) {
		_ = cmd.Process.Kill()
		_ = input.Close()
		_ = output.Close()
		_ = cmd.Wait()
		return nil, errors.New("Windows 项目连接超时")
	}
	admitted = true
	binding.Path = started.Path
	share := &projectShare{cmd: cmd, transport: projectTransport{ReadCloser: bufferedProjectOutput{reader, output}, WriteCloser: input}, done: make(chan struct{}), windowsProject: &binding}
	go func() {
		defer close(share.done)
		_ = reversesshfs.ServeConfinedVirtual(share.transport, hostPath, false, nil)
		share.stop()
		_ = cmd.Wait()
	}()
	return share, nil
}
func validWindowsProjectPath(path, id string) bool {
	return len(path) < 4096 && len(path) > 3 && path[1] == ':' && path[2] == '\\' && !strings.ContainsAny(path, "\x00\r\n") && strings.HasSuffix(path, "\\Cage\\Projects\\"+id) && !strings.Contains(path, "\\..\\")
}
func (s *Server) windowsProjectRequest(ctx context.Context, action string, binding windowsProjectBinding, probe string) (map[string]any, error) {
	cmd, err := guestos.WindowsRelayCommand(ctx, s.profile, "project-mount")
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(map[string]any{"action": action, "id": binding.ID, "grant": binding.Grant, "probe_name": probe})
	if err != nil {
		return nil, err
	}
	cmd.Stdin = bytes.NewReader(append(data, '\n'))
	cmd.Stderr = io.Discard
	var output bytes.Buffer
	cmd.Stdout = &boundedWindowsStatusWriter{buffer: &output, remaining: 16384}
	if err = cmd.Run(); err != nil {
		return nil, errors.New("Windows 项目助手尚未连接")
	}
	var result map[string]any
	decoder := json.NewDecoder(&output)
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result["error"] != nil {
		return nil, errors.New("Windows 项目助手返回无效结果")
	}
	return result, nil
}
func (s *Server) ensureWindowsProjectShare(hostPath string) error {
	s.mu.Lock()
	current := s.projectShares[hostPath]
	s.mu.Unlock()
	if current != nil {
		select {
		case <-current.done:
			current.close()
		default:
			return nil
		}
	}
	probe, err := os.CreateTemp(hostPath, ".cage-mount-probe-*")
	if err != nil {
		return errors.New("无法验证项目目录读写权限")
	}
	defer os.Remove(probe.Name())
	challenge := randomToken()
	if _, err = probe.WriteString(challenge); err != nil {
		probe.Close()
		return err
	}
	if err = probe.Close(); err != nil {
		return err
	}
	binding := windowsProjectIdentity(s.profile, hostPath)
	cmd, err := guestos.WindowsRelayCommand(s.lifetimeContext(), s.profile, "project-mount")
	if err != nil {
		return err
	}
	share, err := startWindowsProjectTransport(cmd, hostPath, binding)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			share.close()
		}
	}()
	ctx, cancel := context.WithTimeout(s.operationContext(), 15*time.Second)
	defer cancel()
	for {
		result, probeErr := s.windowsProjectRequest(ctx, "probe", *share.windowsProject, filepath.Base(probe.Name()))
		if probeErr == nil && result["mounted"] == true && result["id"] == binding.ID && result["path"] == share.windowsProject.Path && result["proof"] == challenge {
			break
		}
		select {
		case <-ctx.Done():
			return errors.New("Windows 项目文件夹连接超时，原有环境和会话保持运行")
		case <-share.done:
			return errors.New("Windows 项目挂载未能连接")
		case <-time.After(100 * time.Millisecond):
		}
	}
	s.mu.Lock()
	if s.projectShares == nil {
		s.projectShares = make(map[string]*projectShare)
	}
	s.projectShares[hostPath] = share
	if s.windowsProjectHistory == nil {
		s.windowsProjectHistory = map[string]string{}
	}
	s.windowsProjectHistory[share.windowsProject.Path] = hostPath
	s.windowsProjectHistory[share.windowsProject.ID] = hostPath
	s.mu.Unlock()
	published = true
	return nil
}
func (s *Server) windowsProjectForHost(hostPath string) (*windowsProjectBinding, error) {

	s.mu.Lock()
	share := s.projectShares[hostPath]
	s.mu.Unlock()
	if share == nil || share.windowsProject == nil {
		return nil, errors.New("Windows 项目尚未授权或已经断开")
	}
	select {
	case <-share.done:
		return nil, errors.New("Windows 项目连接已撤销")
	default:
	}
	binding := *share.windowsProject
	return &binding, nil
}
