package reversesshfs

import (
	"errors"
	"io"
	"os"
	"time"
)

// ConfinedWriteTimeout bounds one complete response packet write. It does not
// expire idle reads or wait for peer EOF, and closes both stream directions.
const ConfinedWriteTimeout = 30 * time.Second

// ErrPinnedRootMismatch means OpenRoot attached a different directory than the
// caller authorized. No SFTP request is served.
var ErrPinnedRootMismatch = errors.New("project root identity changed")

// ServeConfined serves only localPath over an already secured transport. It uses
// the same root and metadata protections as builtin reverse-sshfs mounts.
func ServeConfined(transport io.ReadWriteCloser, localPath string, readonly bool, readonlyNames []string) error {
	server, root, err := newRootedServer(transport, localPath, readonly, readonlyNames)
	if err != nil {
		return err
	}
	defer root.Close()
	defer server.Close()
	return server.Serve()
}

// ServeConfinedPinned serves localPath only when both directories opened for
// the request — os.Root and the rootedSys handle — are the same file as
// identity. Those two opens can race a directory replacement. The comparison
// happens before Serve reads an SFTP handshake or request. Handler behavior is
// otherwise unchanged.
func ServeConfinedPinned(transport io.ReadWriteCloser, localPath string, readonly bool, readonlyNames []string, identity os.FileInfo) error {
	if identity == nil {
		return os.ErrInvalid
	}
	server, root, err := newRootedServer(transport, localPath, readonly, readonlyNames)
	if err != nil {
		return err
	}
	if !root.sameOpenedRoot(identity) {
		_ = server.Close()
		_ = root.Close()
		return ErrPinnedRootMismatch
	}
	defer root.Close()
	defer server.Close()
	return server.Serve()
}

// ServeConfinedVirtual exposes the selected directory at / without disclosing
// its host path. All reads, writes and Guard decisions use the same confined
// root and metadata protections as ServeConfined.
func ServeConfinedVirtual(transport io.ReadWriteCloser, localPath string, readonly bool, readonlyNames []string) error {
	server, root, err := newRootedServerAt(transport, localPath, "/", readonly, readonlyNames)
	if err != nil {
		return err
	}
	defer root.Close()
	defer server.Close()
	return server.Serve()
}
