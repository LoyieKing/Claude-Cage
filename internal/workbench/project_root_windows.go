//go:build windows

package workbench

import (
	"os"

	"golang.org/x/sys/windows"
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
	var fileInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &fileInfo); err != nil {
		return nil, projectRootID{}, err
	}
	return info, projectRootID{Dev: uint64(fileInfo.VolumeSerialNumber), Ino: uint64(fileInfo.FileIndexHigh)<<32 | uint64(fileInfo.FileIndexLow)}, nil
}
