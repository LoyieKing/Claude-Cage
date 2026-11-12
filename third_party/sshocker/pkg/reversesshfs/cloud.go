//go:build linux || darwin || windows

package reversesshfs

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/pkg/sftp"
)

// CloudShare pins one local directory for one authenticated client-initiated
// stream. Only a virtual "/" is visible to the peer. The caller authorizes and
// validates the physical project directory before creating this grant.
type CloudShare struct {
	mu       sync.Mutex
	handlers *cloudHandlers
	stream   io.ReadWriteCloser
	done     chan struct{}
	closed   bool
}

// OpenCloudShare expects an absolute, symlink-resolved path supplied by the local picker.
func OpenCloudShare(directory string, readOnly bool) (*CloudShare, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || filepath.Dir(directory) == directory {
		return nil, os.ErrPermission
	}
	sys, err := openCloudRoot(directory)
	if err != nil {
		return nil, err
	}
	h := &cloudHandlers{&rootedHandlers{rootPath: "/", guardRoot: directory, rootedSys: sys, readonly: readOnly, cloud: true}}
	return &CloudShare{handlers: h}, nil
}

// Serve consumes stream, including when the grant has already been revoked.
func (c *CloudShare) Serve(stream io.ReadWriteCloser) error {
	c.mu.Lock()
	if c.closed || c.stream != nil {
		c.mu.Unlock()
		stream.Close()
		return os.ErrClosed
	}
	c.stream, c.done = stream, make(chan struct{})
	h := c.handlers
	c.mu.Unlock()

	srv := sftp.NewRequestServer(stream, sftp.Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h},
		sftp.WithRSSequential(), sftp.WithRSMaxHandles(64), sftp.WithRSMaxTxPacket(64*1024),
		sftp.WithRSWriteTimeout(ConfinedWriteTimeout))
	err := srv.Serve()
	stream.Close()
	c.mu.Lock()
	h.rootedSys.close()
	c.closed = true
	close(c.done)
	c.mu.Unlock()
	return err
}

// Close revokes the grant and waits until buffered requests and open file
// handles have been retired. A closed grant cannot attach to another stream.
func (c *CloudShare) Close() error {
	c.mu.Lock()
	if c.stream == nil {
		defer c.mu.Unlock()
		if c.closed {
			return nil
		}
		c.closed = true
		return c.handlers.rootedSys.close()
	}
	stream, done, closed := c.stream, c.done, c.closed
	c.closed = true
	c.mu.Unlock()
	if !closed {
		stream.Close()
	}
	<-done
	return nil
}

// Use the confined server's handle-relative writes. All reads and metadata
// requests use the same no-follow path walk, including the leaf. An internal
// symlink could otherwise expose a protected path under another name.
type cloudHandlers struct{ *rootedHandlers }

func (h *cloudHandlers) accessible(p string) (string, error) {
	rel, err := h.rel(p)
	if err != nil || !writableName(rel) {
		return "", errDenied
	}
	return rel, nil
}

// One desktop process can serve several grants. Bound long-lived descriptors
// across all of them, leaving room for the SSH connections and the workbench.
var cloudSlots = make(chan struct{}, 128)

func acquireCloudSlot() error {
	select {
	case cloudSlots <- struct{}{}:
		return nil
	default:
		return errors.New("too many open project files")
	}
}

type cloudFile struct {
	writableFile
	writable bool
}

func (f cloudFile) Close() error {
	err := f.File.Close()
	<-cloudSlots
	return err
}

func (h *cloudHandlers) fileHandle(r *sftp.Request, write bool) (cloudFile, error) {
	if err := acquireCloudSlot(); err != nil {
		return cloudFile{}, err
	}
	var f writableFile
	var err error
	if write {
		f, err = h.openWritable(r)
	} else {
		var rel string
		if rel, err = h.accessible(r.Filepath); err == nil {
			f.File, err = h.cloudOpenRead(rel)
		}
	}
	if err != nil {
		<-cloudSlots
		return cloudFile{}, err
	}
	return cloudFile{writableFile: f, writable: write}, nil
}

func (h *cloudHandlers) Fileread(r *sftp.Request) (io.ReaderAt, error)  { return h.fileHandle(r, false) }
func (h *cloudHandlers) Filewrite(r *sftp.Request) (io.WriterAt, error) { return h.fileHandle(r, true) }
func (h *cloudHandlers) OpenFile(r *sftp.Request) (sftp.WriterAtReaderAt, error) {
	return h.fileHandle(r, true)
}

func (h *cloudHandlers) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	rel, err := h.accessible(r.Filepath)
	if err != nil {
		return nil, err
	}
	if r.Method == "Stat" {
		if file, ok := r.HandleFile().(interface{ Stat() (os.FileInfo, error) }); ok {
			info, err := file.Stat()
			if err != nil {
				return nil, err
			}
			return listerAt{info}, nil
		}
	}
	f, err := h.cloudOpenRead(rel)
	if err != nil {
		return nil, err
	}
	if r.Method == "List" {
		if err := acquireCloudSlot(); err != nil {
			f.Close()
			return nil, err
		}
		return &dirLister{f: f, cloud: true}, nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return listerAt{info}, nil
}

func (h *cloudHandlers) Lstat(r *sftp.Request) (sftp.ListerAt, error) { return h.Filelist(r) }
func (h *cloudHandlers) Readlink(string) (string, error)              { return "", errDenied }
func (h *cloudHandlers) RealPath(p string) (string, error) {
	rel, err := h.accessible(p)
	if err != nil {
		return "", err
	}
	return path.Join("/", rel), nil
}

func (h *cloudHandlers) Filecmd(r *sftp.Request) error {
	switch r.Method {
	case "Setstat":
		if h.readonly || r.AttrFlags().UidGid {
			return errDenied
		}
		if f, ok := r.HandleFile().(cloudFile); ok && f.writable {
			return h.fsetstat(f.File, r)
		}
		// A read handle cannot acquire write access through FSETSTAT.
		if r.HandleFile() != nil {
			return errDenied
		}
		f, err := h.openFile(&sftp.Request{Filepath: r.Filepath, Flags: 2 /* SSH_FXF_WRITE */})
		if err != nil {
			return err
		}
		defer f.Close()
		return h.fsetstat(f, r)
	case "Rename":
		return h.rename(r, true)
	case "Remove", "Rmdir":
		return h.remove(r)
	case "Mkdir":
		return h.mkdir(r)
	default:
		// Refuse both hard-link and symbolic-link creation.
		return errDenied
	}
}

// The remote environment owns its own disk reports; querying the desktop
// volume via an SFTP extension would disclose unrelated host information.
func (h *cloudHandlers) StatVFS(*sftp.Request) (*sftp.StatVFS, error) {
	return nil, sftp.ErrSSHFxOpUnsupported
}

func (h *cloudHandlers) PosixRename(r *sftp.Request) error { return h.rename(r, false) }

// MatchesRoot compares the directory as picked before validation with the one
// actually held by this grant. Replacing the picker path invalidates the grant.
func (c *CloudShare) MatchesRoot(selected os.FileInfo) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || selected == nil {
		return false
	}
	f, err := c.handlers.cloudOpenRead(".")
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	return err == nil && info.IsDir() && os.SameFile(info, selected)
}
