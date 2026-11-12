//go:build linux

package containerenv

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// processStarted matches the host cage start token: boot id plus the process
// starttime. An empty result cannot prove PID reuse.
func processStarted(pid int) string {
	if pid <= 0 {
		return ""
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	index := bytes.LastIndexByte(stat, ')')
	if index < 0 {
		return ""
	}
	fields := strings.Fields(string(stat[index+1:]))
	if len(fields) < 20 {
		return ""
	}
	return strings.TrimSpace(string(boot)) + ":" + fields[19]
}

func processAlive(pid int, started string) bool {
	if pid <= 1 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	if started == "" {
		return true
	}
	current := processStarted(pid)
	return current == "" || current == started
}

func (r *Runtime) networkLockHeld() (bool, error) {
	release, err := tryLockRuntime(filepath.Join(r.InstanceDir(), "network.lock"))
	if err == nil {
		release()
		return false, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return true, nil
	}
	return false, err
}
