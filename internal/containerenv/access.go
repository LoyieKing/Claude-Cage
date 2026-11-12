package containerenv

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/host"
	"golang.org/x/crypto/ssh"
)

func (r *Runtime) ensureSSHKey() ([]byte, error) {
	if err := host.EnsurePrivateDir(r.InstanceDir()); err != nil {
		return nil, err
	}
	path := filepath.Join(r.InstanceDir(), "id_ed25519")
	if data, err := os.ReadFile(path); err == nil {
		if err := host.CheckPrivateFile(path); err != nil {
			return nil, err
		}
		key, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return nil, errors.New("容器 SSH 密钥无效；未覆盖现有密钥")
		}
		return ssh.MarshalAuthorizedKey(key.PublicKey()), nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(private, "Cage container")
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	err = pem.Encode(file, block)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	key, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		return nil, err
	}
	return ssh.MarshalAuthorizedKey(key), nil
}

// InstallAccess provisions only this runtime's generated public key. Private
// SSH keys stay in the host profile. It does not expose a container port.
func (r *Runtime) InstallAccess(ctx context.Context) error {
	inspection, err := r.Inspect(ctx)
	if err != nil {
		return err
	}
	if !inspection.State.Running {
		return errors.New("请先启动容器")
	}
	public, err := r.ensureSSHKey()
	if err != nil {
		return err
	}
	command := r.command(ctx, "exec", "-i", "--user", "0", inspection.State.ContainerID, "/bin/sh", "-c", `set -eu
test -d /home/cage
umask 077
cat > /etc/ssh/cage_authorized_keys
chmod 0644 /etc/ssh/cage_authorized_keys
ssh-keygen -A
cat /etc/ssh/ssh_host_ed25519_key.pub
`)
	command.Stdin = bytes.NewReader(public)
	var output bytes.Buffer
	command.Stdout = &limitedWriter{writer: &output, left: 4096}
	if err := command.Run(); err != nil {
		return errors.New("无法配置容器 SSH 访问")
	}
	key, _, _, rest, err := ssh.ParseAuthorizedKey(output.Bytes())
	if err != nil || len(bytes.TrimSpace(rest)) != 0 || key.Type() != ssh.KeyAlgoED25519 {
		return errors.New("容器 SSH 主机密钥无效")
	}
	line := "cage-container-" + r.identity + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + "\n"
	path := filepath.Join(r.InstanceDir(), "known_hosts")
	if old, err := os.ReadFile(path); err == nil {
		if err := host.CheckPrivateFile(path); err != nil {
			return err
		}
		if !bytes.Equal(old, []byte(line)) {
			return errors.New("容器 SSH 主机密钥已变化；拒绝覆盖信任记录")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, []byte(line), 0600)
}

func sshConfigValue(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("SSH 配置路径无效")
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`, nil
}

func (r *Runtime) writeSSHConfig(port int) error {
	if port < 1024 || port > 65535 {
		return errors.New("容器 SSH 入口端口无效")
	}
	identity, err := sshConfigValue(filepath.Join(r.InstanceDir(), "id_ed25519"))
	if err != nil {
		return err
	}
	known, err := sshConfigValue(filepath.Join(r.InstanceDir(), "known_hosts"))
	if err != nil {
		return err
	}
	data := fmt.Sprintf("Host cage-container\n HostName 127.0.0.1\n Port %d\n User cage\n IdentityFile %s\n IdentitiesOnly yes\n HostKeyAlias cage-container-%s\n HostKeyAlgorithms ssh-ed25519\n UserKnownHostsFile %s\n StrictHostKeyChecking yes\n PasswordAuthentication no\n KbdInteractiveAuthentication no\n ForwardAgent no\n ForwardX11 no\n PermitLocalCommand no\n", port, identity, r.identity, known)
	return os.WriteFile(r.SSHConfig(), []byte(data), 0600)
}

// Stop retains the complete container filesystem and host profile. Only the
// exact, labelled container of this profile is ever stopped; no rm is issued.
func (r *Runtime) Stop(ctx context.Context) error {
	if err := host.EnsurePrivateDir(r.InstanceDir()); err != nil {
		return err
	}
	unlock, err := lockRuntime(ctx, filepath.Join(r.InstanceDir(), "lifecycle.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	// Persist Stop before touching the container so a late Recover cannot
	// observe the old running intent and launch another worker.
	if _, err := r.advanceIntent(intentStopped, ""); err != nil {
		return err
	}
	rootless, _, err := r.engineSecurity(ctx)
	if err != nil {
		return err
	}
	if !rootless {
		return errors.New("拒绝操作非 rootless 容器引擎")
	}
	container, err := r.inspectContainer(ctx)
	if err != nil {
		return err
	}
	if container == nil {
		return nil
	}
	if client, err := cage.OpenGateClient(r.InstanceDir()); err == nil {
		closeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, _ = client.Close(closeCtx, "容器正在停止")
		cancel()
	}
	if container.State.Running {
		if _, err = r.output(ctx, "stop", "--time", "20", container.ID); err != nil {
			return err
		}
	}
	// Wait for the old network owner's deferred cleanup before allowing another
	// Start to publish new credentials or runtime records in the same directory.
	cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	release, err := lockRuntime(cleanupCtx, filepath.Join(r.InstanceDir(), "network.lock"))
	if err != nil {
		return errors.New("容器已停止，网络服务仍在退出；原文件保留，请稍后重试")
	}
	defer release()
	for _, name := range []string{"network.json", "cage-running.json"} {
		if err := os.Remove(filepath.Join(r.InstanceDir(), name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
