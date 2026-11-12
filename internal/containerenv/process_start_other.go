//go:build !linux

package containerenv

import "errors"

func processStarted(int) string { return "" }

func processAlive(int, string) bool { return false }

func (r *Runtime) networkLockHeld() (bool, error) {
	return false, errors.New("容器运行环境仅支持 Linux 服务器")
}
