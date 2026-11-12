//go:build linux

package containerenv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// RunTAP is an internal guest-only helper. Its stdout is exclusively framed
// Ethernet data. The caller connects it to the host-owned network stack.
func RunTAP(ctx context.Context, input io.Reader, output io.Writer) error {
	if err := requirePrivateGuestNetwork(); err != nil {
		return err
	}
	// Exclusive ownership of the guest TAP. Never steal another helper's
	// descriptor or delete its interface; wait only after this process holds
	// the lock and the previous owner has exited.
	lockFile, err := os.OpenFile("/run/cage-tap.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("容器 TAP 已由其他进程持有")
	}
	defer unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
	// A disconnected engine exec can take a moment to deliver EOF to its old
	// helper. Wait for that non-persistent TAP to vanish; never delete an
	// interface or take over another process's file descriptor.
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		interfaces, err := net.Interfaces()
		if err != nil {
			return err
		}
		oldTap := false
		for _, iface := range interfaces {
			if iface.Name == TapName {
				oldTap = true
			} else if iface.Name != "lo" {
				return errors.New("容器存在额外网络接口，拒绝启动 TAP")
			}
		}
		if !oldTap {
			break
		}
		select {
		case <-waitCtx.Done():
			return errors.New("旧容器网络接口仍未退出，请稍后重连")
		case <-time.After(100 * time.Millisecond):
		}
	}
	// An unattached TUN fd reports POLLERR. Registering it with Go's poller
	// before TUNSETIFF races with an irreversible "not pollable" error.
	// Attach the device first, then wrap the nonblocking descriptor.
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	request, err := unix.NewIfreq(TapName)
	if err != nil {
		_ = unix.Close(fd)
		return err
	}
	request.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, request); err != nil {
		_ = unix.Close(fd)
		return err
	}
	file := os.NewFile(uintptr(fd), "/dev/net/tun")
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("无法管理容器 TAP 文件描述符")
	}
	defer file.Close()
	for _, arguments := range [][]string{
		{"link", "set", "dev", TapName, "address", GuestMAC, "mtu", fmt.Sprint(NetworkMTU)},
		{"link", "set", "dev", "lo", "up"},
		{"address", "add", GuestIP + "/24", "dev", TapName},
		{"link", "set", "dev", TapName, "up"},
		{"route", "add", "default", "via", GatewayIP, "dev", TapName},
	} {
		if result, err := exec.CommandContext(ctx, "ip", arguments...).CombinedOutput(); err != nil {
			return fmt.Errorf("configure container TAP: %w: %.1024s", err, result)
		}
	}
	// The host resolver must never be inherited by the workload. Write through
	// the engine's resolv.conf mount; do not replace the mount or a symlink.
	info, err := os.Lstat("/etc/resolv.conf")
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("容器 DNS 配置必须是普通文件")
	}
	if err = os.WriteFile("/etc/resolv.conf", []byte("nameserver "+GatewayIP+"\noptions timeout:2 attempts:2\n"), 0644); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = file.Close() })
	defer stop()
	ended := make(chan error, 1)
	go func() {
		buffer := make([]byte, maxFrameSize)
		for {
			frame, err := readFrame(input, buffer)
			if err == nil {
				var n int
				n, err = file.Write(frame)
				if err == nil && n != len(frame) {
					err = io.ErrShortWrite
				}
			}
			if err != nil {
				_ = file.Close()
				ended <- err
				return
			}
		}
	}()
	buffer := make([]byte, maxFrameSize)
	for {
		n, err := file.Read(buffer)
		if err == nil {
			err = writeFrame(output, buffer[:n])
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			select {
			case inputErr := <-ended:
				return inputErr
			default:
				return err
			}
		}
	}
}

func requirePrivateGuestNetwork() error {
	if os.Geteuid() != 0 {
		return errors.New("容器 TAP helper 必须以容器内 root 运行")
	}
	uidMap, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		return err
	}
	fields := strings.Fields(string(uidMap))
	if len(fields) == 3 && fields[0] == "0" && fields[1] == "0" && fields[2] == "4294967295" {
		return errors.New("容器 TAP helper 拒绝在宿主用户命名空间运行")
	}
	if _, first := os.Stat("/run/.containerenv"); first != nil {
		if _, second := os.Stat("/.dockerenv"); second != nil {
			return errors.New("容器 TAP helper 只能在 Cage 容器内运行")
		}
	}
	return nil
}
