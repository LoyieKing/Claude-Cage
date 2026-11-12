package workbench

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/containerenv"
	"github.com/sylvinus/agent-vm/internal/host"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// readLimaHostKey reads the guest host public key through the legacy managed
// OpenSSH bootstrap. Lima's generated config does not pin host keys
// (StrictHostKeyChecking=no, UserKnownHostsFile=/dev/null,
// NoHostAuthenticationForLocalhost=yes). This read is not an independent or
// authenticated proof of the guest. The key is stored once and later dials
// require it without rewriting it. Containers do not use this path.
var readLimaHostKey = func(s *Server, ctx context.Context) ([]byte, error) {
	return s.guest(ctx, "cat /etc/ssh/ssh_host_ed25519_key.pub")
}

// dialGuestSSH opens a native SSH client to the Linux guest (Lima or container).
// Callers own the returned client and must Close it. The dial is only to
// 127.0.0.1, with the profile identity and a pinned host key. macOS and Windows
// guests, proxy/agent fallbacks, and a changed host key return an error.
func (s *Server) dialGuestSSH(ctx context.Context) (*ssh.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.windowsGuest != nil || s.macosGuest != nil {
		return nil, errors.New("当前客机不使用 Linux SSH")
	}
	plan, err := s.linuxGuestSSHPlan(ctx)
	if err != nil {
		return nil, err
	}
	return dialPinnedGuestSSH(ctx, plan)
}

type guestSSHPlan struct {
	user       string
	identity   string
	knownHosts string
	alias      string
	port       int
}

func (s *Server) linuxGuestSSHPlan(ctx context.Context) (guestSSHPlan, error) {
	configPath := filepath.Join(s.instanceDir(), "ssh.config")
	if err := host.CheckPrivateFile(configPath); err != nil {
		return guestSSHPlan{}, errors.New("客机 SSH 配置不可用")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return guestSSHPlan{}, errors.New("客机 SSH 配置不可用")
	}
	if len(data) > 64<<10 {
		return guestSSHPlan{}, errors.New("客机 SSH 配置无效")
	}
	parsed, err := parseGuestSSHConfig(data, s.sshTarget())
	if err != nil {
		return guestSSHPlan{}, err
	}
	if !pathInside(s.profile, parsed.identityFile) {
		return guestSSHPlan{}, errors.New("客机 SSH 密钥不在当前配置中")
	}
	if err := host.CheckPrivateFile(parsed.identityFile); err != nil {
		return guestSSHPlan{}, errors.New("客机 SSH 密钥不可用")
	}
	plan := guestSSHPlan{user: parsed.user, identity: parsed.identityFile, port: parsed.port, alias: parsed.hostKeyAlias}
	if plan.alias == "" {
		plan.alias = s.sshTarget()
	}
	if containerenv.Configured(s.profile) {
		if !parsed.strict {
			return guestSSHPlan{}, errors.New("容器 SSH 必须固定主机身份")
		}
		if parsed.knownHosts == "" || !pathInside(s.profile, parsed.knownHosts) {
			return guestSSHPlan{}, errors.New("容器 SSH 主机身份记录不可用")
		}
		if err := validateDedicatedKnownHosts(parsed.knownHosts, plan.alias); err != nil {
			return guestSSHPlan{}, errors.New("容器 SSH 主机身份记录无效")
		}
		plan.knownHosts = parsed.knownHosts
		return plan, nil
	}
	// Lima's generated file disables checking. Ignore those directives and pin
	// a dedicated known_hosts instead of dialing without a host key.
	known := filepath.Join(s.instanceDir(), "known_hosts")
	if err := validateDedicatedKnownHosts(known, plan.alias); err != nil {
		if !os.IsNotExist(err) {
			return guestSSHPlan{}, errors.New("客机 SSH 主机身份已变化或记录无效")
		}
		if err := s.provisionLimaKnownHosts(ctx, known, plan.alias); err != nil {
			return guestSSHPlan{}, err
		}
	}
	plan.knownHosts = known
	return plan, nil
}

func (s *Server) provisionLimaKnownHosts(ctx context.Context, path, alias string) error {
	output, err := readLimaHostKey(s, ctx)
	if err != nil {
		return errors.New("无法通过现有管理通道读取客机 SSH 主机公钥")
	}
	key, _, _, rest, err := ssh.ParseAuthorizedKey(output)
	if err != nil || len(bytes.TrimSpace(rest)) != 0 || key == nil || key.Type() != ssh.KeyAlgoED25519 {
		return errors.New("客机 SSH 主机公钥无效")
	}
	line := alias + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + "\n"
	if err := host.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			if validateDedicatedKnownHosts(path, alias) != nil {
				return errors.New("客机 SSH 主机身份已变化或记录无效")
			}
			return nil
		}
		return err
	}
	_, writeErr := file.WriteString(line)
	closeErr := file.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = os.Remove(path)
		return writeErr
	}
	if err := host.ProtectFile(path); err != nil {
		return err
	}
	return validateDedicatedKnownHosts(path, alias)
}

func dialPinnedGuestSSH(ctx context.Context, plan guestSSHPlan) (*ssh.Client, error) {
	private, err := os.ReadFile(plan.identity)
	if err != nil {
		return nil, errors.New("客机 SSH 密钥不可用")
	}
	signer, err := ssh.ParsePrivateKey(private)
	clear(private)
	if err != nil {
		return nil, errors.New("客机 SSH 密钥不可用")
	}
	callback, err := knownhosts.New(plan.knownHosts)
	if err != nil {
		return nil, errors.New("客机 SSH 主机身份记录无效")
	}
	config := &ssh.ClientConfig{
		User:              plan.user,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback:   callback,
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		Timeout:           3 * time.Second,
	}
	dialer := net.Dialer{Timeout: 3 * time.Second}
	// Generated configs name a loopback forward. Never dial the configured
	// hostname; it was already required to be 127.0.0.1.
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(plan.port)))
	if err != nil {
		return nil, errors.New("无法连接客机 SSH")
	}
	handshake, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	stop := context.AfterFunc(handshake, func() { _ = connection.Close() })
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	// known_hosts is keyed by the stable alias, not the ephemeral forward port.
	transport, channels, requests, err := ssh.NewClientConn(connection, net.JoinHostPort(plan.alias, "22"), config)
	stopped := stop()
	if err != nil {
		_ = connection.Close()
		var mismatch *knownhosts.KeyError
		if errors.As(err, &mismatch) {
			return nil, errors.New("客机 SSH 主机身份不匹配")
		}
		return nil, errors.New("客机 SSH 握手失败")
	}
	// The close callback may already have run if the handshake context ended
	// as the client became ready. Never hand that connection to the caller.
	if !stopped || ctx.Err() != nil || handshake.Err() != nil {
		_ = transport.Close()
		_ = connection.Close()
		return nil, errors.New("客机 SSH 握手已取消")
	}
	_ = connection.SetDeadline(time.Time{})
	return ssh.NewClient(transport, channels, requests), nil
}

type parsedGuestSSH struct {
	user          string
	identityFile  string
	knownHosts    string
	hostKeyAlias  string
	port          int
	strict        bool
	identityCount int
}

// parseGuestSSHConfig reads only the generated profile config. It does not
// evaluate Include, Match, or command directives. Those directives are rejected
// anywhere in the file — before the first Host, inside it, and in later blocks —
// because the legacy OpenSSH bootstrap evaluates the whole file. Proxy, agent,
// and extra-identity fallbacks are rejected the same way. Host-key weakening
// directives are not applied.
func parseGuestSSHConfig(data []byte, target string) (parsedGuestSSH, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	var parsed parsedGuestSSH
	var matched, applying, preamble, consumed bool
	applying, preamble = true, true
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := splitSSHKeyword(line)
		if !ok {
			continue
		}
		if strings.EqualFold(key, "Host") {
			if applying && !preamble {
				consumed = true
			}
			preamble = false
			applying = !consumed && sshHostMatches(value, target)
			if applying {
				matched = true
			}
			continue
		}
		if err := rejectSSHFallback(key, value); err != nil {
			return parsedGuestSSH{}, err
		}
		if !applying {
			continue
		}
		switch strings.ToLower(key) {
		case "hostname":
			if value != "127.0.0.1" {
				return parsedGuestSSH{}, errors.New("客机 SSH 只能连接本机回环地址")
			}
		case "port":
			port, err := strconv.Atoi(value)
			if err != nil || port < 1 || port > 65535 || parsed.port != 0 {
				return parsedGuestSSH{}, errors.New("客机 SSH 端口无效")
			}
			parsed.port = port
		case "user":
			if !safeSSHToken(value) || parsed.user != "" {
				return parsedGuestSSH{}, errors.New("客机 SSH 用户无效")
			}
			parsed.user = value
		case "identityfile":
			parsed.identityCount++
			if parsed.identityCount > 1 {
				return parsedGuestSSH{}, errors.New("客机 SSH 不接受额外密钥")
			}
			if !filepath.IsAbs(value) {
				return parsedGuestSSH{}, errors.New("客机 SSH 密钥路径无效")
			}
			parsed.identityFile = filepath.Clean(value)
		case "userknownhostsfile":
			if value == os.DevNull || value == "/dev/null" {
				continue
			}
			if !filepath.IsAbs(value) {
				return parsedGuestSSH{}, errors.New("客机 SSH 主机身份路径无效")
			}
			parsed.knownHosts = filepath.Clean(value)
		case "hostkeyalias":
			if !safeSSHToken(value) {
				return parsedGuestSSH{}, errors.New("客机 SSH 主机别名无效")
			}
			parsed.hostKeyAlias = value
		case "hostkeyalgorithms":
			if !sshAlgorithmAllowsEd25519(value) {
				return parsedGuestSSH{}, errors.New("客机 SSH 主机密钥算法不受支持")
			}
		case "stricthostkeychecking":
			parsed.strict = strings.EqualFold(value, "yes")
			if !parsed.strict && !strings.EqualFold(value, "no") {
				return parsedGuestSSH{}, errors.New("客机 SSH 主机身份校验无效")
			}
		case "identitiesonly":
			if !strings.EqualFold(value, "yes") {
				return parsedGuestSSH{}, errors.New("客机 SSH 不接受额外密钥")
			}
		case "passwordauthentication", "kbdinteractiveauthentication":
			if !strings.EqualFold(value, "no") {
				return parsedGuestSSH{}, errors.New("客机 SSH 不接受口令认证")
			}
		default:
			// Generated Lima options such as compression and ciphers are not
			// connection policy. Unknown directives are ignored because this
			// client never evaluates them.
		}
	}
	if !matched || parsed.port == 0 || parsed.user == "" || parsed.identityFile == "" {
		return parsedGuestSSH{}, errors.New("客机 SSH 配置不完整")
	}
	return parsed, nil
}

// rejectSSHFallback rejects directives the legacy OpenSSH client would act on
// even when they sit outside the selected Host block.
func rejectSSHFallback(key, value string) error {
	switch strings.ToLower(key) {
	case "include", "match", "localcommand", "remotecommand", "proxyusefdpass", "pkcs11provider", "securitykeyprovider", "certificatefile":
		return errors.New("客机 SSH 配置无效")
	case "proxycommand", "proxyjump":
		if !strings.EqualFold(value, "none") {
			return errors.New("客机 SSH 不接受代理跳转")
		}
	case "forwardagent":
		if !strings.EqualFold(value, "no") {
			return errors.New("客机 SSH 不接受代理转发")
		}
	case "identityagent":
		if !strings.EqualFold(value, "none") {
			return errors.New("客机 SSH 不接受密钥代理")
		}
	case "permitlocalcommand":
		if !strings.EqualFold(value, "no") {
			return errors.New("客机 SSH 配置无效")
		}
	case "globalknownhostsfile":
		if value != os.DevNull && value != "/dev/null" {
			return errors.New("客机 SSH 不接受额外主机身份")
		}
	}
	return nil
}

func validateDedicatedKnownHosts(path, alias string) error {
	if err := host.CheckPrivateFile(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > 64<<10 {
		return errors.New("invalid known hosts")
	}
	var keys int
	rest := data
	for {
		marker, hosts, pub, _, next, err := ssh.ParseKnownHosts(rest)
		if err == io.EOF {
			break
		}
		if err != nil || marker != "" || pub == nil || pub.Type() != ssh.KeyAlgoED25519 || len(hosts) != 1 || hosts[0] != alias {
			return errors.New("invalid known hosts")
		}
		keys++
		rest = next
	}
	if keys != 1 {
		return errors.New("invalid known hosts")
	}
	return nil
}

func splitSSHKeyword(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	i := 0
	for i < len(line) && line[i] != ' ' && line[i] != '\t' && line[i] != '=' {
		i++
	}
	key := line[:i]
	rest := strings.TrimSpace(line[i:])
	if strings.HasPrefix(rest, "=") {
		rest = strings.TrimSpace(rest[1:])
	}
	if key == "" || rest == "" {
		return "", "", false
	}
	return key, unquoteSSHValue(rest), true
}

func unquoteSSHValue(value string) string {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(value[1 : len(value)-1])
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1]
	}
	return value
}

func sshHostMatches(pattern, target string) bool {
	pattern = strings.TrimSpace(pattern)
	if strings.ContainsAny(pattern, " \t*?") && pattern != "*" {
		return false
	}
	return pattern == target || pattern == "*"
}

func safeSSHToken(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func sshAlgorithmAllowsEd25519(value string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.TrimSpace(part) == ssh.KeyAlgoED25519 {
			return true
		}
	}
	return false
}

func pathInside(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
