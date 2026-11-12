//go:build unix

package containerenv

import (
	"os"

	"golang.org/x/sys/unix"
)

// captureHostRoot opens the real directory. The FileInfo is the identity
// ServeConfinedPinned compares against both of its opens; dev/ino is the
// persisted pin and is not a substitute for that comparison.
func captureHostRoot(path string) (os.FileInfo, uint64, uint64, error) {
	link, err := os.Lstat(path)
	if err != nil {
		return nil, 0, 0, err
	}
	if link.Mode()&os.ModeSymlink != 0 || !link.IsDir() {
		return nil, 0, 0, errShareBlocked
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, 0, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return nil, 0, 0, err
	}
	if !info.IsDir() {
		return nil, 0, 0, errShareBlocked
	}
	return info, uint64(stat.Dev), uint64(stat.Ino), nil
}
