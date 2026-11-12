//go:build !unix

package containerenv

import (
	"os"
)

func captureHostRoot(path string) (os.FileInfo, uint64, uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, 0, err
	}
	if !info.IsDir() {
		return nil, 0, 0, errShareBlocked
	}
	return info, 0, 0, errShareBlocked
}
