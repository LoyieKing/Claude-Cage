package remotehost

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/deploy/cloud"
	"golang.org/x/crypto/ssh"
)

func runSSH(client *ssh.Client, command string, input io.Reader) ([]byte, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()
	out := &limitedOutput{limit: 64 << 10}
	session.Stdout, session.Stderr, session.Stdin = out, io.Discard, input
	err = session.Run(command)
	return out.Bytes(), err
}

func lifecycleCommand(p HostProfile, action string, extra ...string) string {
	args := []string{"bash", "-c", cloud.Lifecycle, "cage-cloud", action, p.Executable, p.ConfigPath}
	args = append(args, extra...)
	for i := range args {
		args[i] = shellQuote(args[i])
	}
	return strings.Join(args, " ")
}

func deploymentError(err error) error {
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		messages := map[int]string{
			61: "需要 Linux x86_64 服务器，请使用普通 SSH 用户登录",
			62: "服务器缺少 rootless 容器所需的用户映射、工具、FUSE、TUN 或 cgroup v2",
			63: "SSH 用户的 systemd 用户服务不可用",
			64: "自动部署需要由字母、数字、点、短横线、下划线和斜杠组成的绝对配置路径",
			65: "目标目录或服务已属于其他配置。请选择空的配置目录，或填写已安装的 Cage 程序路径",
			66: "找不到指定的 Cage 程序。首次自动部署请将程序路径设为 cage",
			67: "此服务器正在部署 Cage，请稍后重新连接",
			68: "配置目录所在磁盘需要至少 7 GiB 可用空间",
			69: "部署包传输校验失败，请重新部署",
			70: "服务器的独立容器引擎启动失败，请检查用户服务日志和 cgroup 委派",
			71: "服务器无法加载或校验工作环境镜像，请检查磁盘空间",
			72: "服务器准备工作环境失败，可查看配置目录中的 cloud-host/setup.log 后重试",
			74: "下载容器引擎失败，请检查服务器的网络后重试",
			75: "服务器无法获取固定版本的 Debian 基础镜像，请检查云端网络后重试",
			76: "服务器安装工具失败。详细日志位于配置目录的 cloud-host/build 下",
			73: "云端 Cage 用户服务启动失败，请检查服务器用户服务日志",
		}
		if message := messages[exit.ExitStatus()]; message != "" {
			return failure("cloud_setup_failed", message)
		}
	}
	return failure("server_entry_failed", "云端 Cage 未就绪，请检查服务器上的 Cage 程序、配置路径和用户服务")
}

// SSH reads the archive as a stream. Report whole percentages without loading it into memory.
type uploadProgress struct {
	size, sent int64
	last       int
	phase      func(string)
}

func (p *uploadProgress) Write(data []byte) (int, error) {
	p.sent += int64(len(data))
	percent := int(p.sent * 100 / p.size)
	if percent != p.last {
		p.last = percent
		if percent == 100 {
			p.phase("cloud_install")
		} else {
			p.phase("cloud_upload_" + strconv.Itoa(percent))
		}
	}
	return len(data), nil
}

func cloudEntry(ctx context.Context, client *ssh.Client, p HostProfile, deploy, attachOnly bool, phase func(string)) ([]byte, error) {
	command, err := ServerEntryCommand(p)
	if err != nil {
		return nil, err
	}
	if out, err := runSSH(client, command, nil); err == nil {
		return out, nil
	} else if attachOnly {
		return nil, failure("service_unavailable", "云端 Cage 未在运行。重新连接不会部署、升级或启动环境。")
	}
	phase("cloud_detect")
	out, err := runSSH(client, lifecycleCommand(p, "probe"), nil)
	if err != nil {
		return nil, deploymentError(err)
	}
	switch string(out) {
	case "missing":
		if !deploy {
			return nil, failure("deployment_required", "此主机尚未部署 Cage。部署将在 SSH 用户的目录中安装 Cage 和独立容器环境。")
		}
		binary, err := os.Executable()
		if err != nil {
			return nil, err
		}
		payload := filepath.Join(filepath.Dir(binary), "cloud-linux-amd64.tar")
		sum, err := os.ReadFile(payload + ".sha256")
		digest := strings.TrimSpace(string(sum))
		if err != nil || !regexp.MustCompile("^[a-f0-9]{64}$").MatchString(digest) {
			return nil, failure("payload_missing", "此客户端缺少云端部署包，请安装包含部署包的 Cage")
		}
		file, err := os.Open(payload)
		if err != nil {
			return nil, failure("payload_missing", "无法读取云端部署包，请检查 Cage 安装")
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return nil, failure("payload_missing", "云端部署包无效，请检查 Cage 安装")
		}
		input := io.TeeReader(file, &uploadProgress{size: info.Size(), phase: phase})
		phase("cloud_upload_0")
		if _, err = runSSH(client, lifecycleCommand(p, "install", digest), input); err != nil {
			return nil, deploymentError(err)
		}
	case "manual", "managed":
		if out, err := runSSH(client, lifecycleCommand(p, "entry"), nil); err == nil {
			return out, nil
		}
		phase("cloud_start")
		if _, err := runSSH(client, lifecycleCommand(p, "start"), nil); err != nil {
			return nil, deploymentError(err)
		}
	default:
		return out, nil
	}
	phase("server_handshake")
	for i := 0; i < 40; i++ {
		if out, err := runSSH(client, lifecycleCommand(p, "entry"), nil); err == nil {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return nil, failure("server_start_timeout", "云端服务已请求启动，但连接入口尚未就绪，请检查用户服务日志后重连")
}
