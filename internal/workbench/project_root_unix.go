//go:build unix

package workbench

import (
	"os"

	"golang.org/x/sys/unix"
)

func readProjectRootID(path string) (projectRootID, error) {
	_, id, err := openProjectRoot(path)
	return id, err
}

func openProjectRoot(path string) (os.FileInfo, projectRootID, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, projectRootID{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, projectRootID{}, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &st); err != nil {
		return nil, projectRootID{}, err
	}
	return info, projectRootID{Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, nil
}
