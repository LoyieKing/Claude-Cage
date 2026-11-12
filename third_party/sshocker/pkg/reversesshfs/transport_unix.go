//go:build linux || darwin

package reversesshfs

import (
	"io"
	"net"
	"os"
	"os/exec"
	"sync"

	"golang.org/x/sys/unix"
)

// Give SSH the same open file description for stdin and stdout, matching
// sshfs's own start_ssh socketpair. SSH's nonblocking stdin then also applies
// to stdout; a full output buffer cannot trap the relay in a blocking write
// while the SFTP server waits for it to consume a response.
func prepareBuiltinTransport(command *exec.Cmd) (io.ReadWriteCloser, func(), error) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	unix.CloseOnExec(pair[0])
	unix.CloseOnExec(pair[1])
	parent := os.NewFile(uintptr(pair[0]), "cage-sftp-parent")
	child := os.NewFile(uintptr(pair[1]), "cage-sftp-child")
	connection, err := net.FileConn(parent)
	_ = parent.Close()
	if err != nil {
		_ = child.Close()
		return nil, nil, err
	}
	command.Stdin, command.Stdout = child, child
	var once sync.Once
	release := func() { once.Do(func() { _ = child.Close() }) }
	return connection, release, nil
}
