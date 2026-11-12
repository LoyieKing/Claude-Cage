package workbench

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/lima-vm/sshocker/pkg/reversesshfs"
)

type observedProjectReader struct {
	io.ReadCloser
	entered chan struct{}
	once    sync.Once
}

func (r *observedProjectReader) Read(data []byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	return r.ReadCloser.Read(data)
}

func TestProjectShareCloseWaitsForTransportAndChild(t *testing.T) {
	const helper = "CAGE_TEST_PROJECT_SHARE_CHILD"
	if os.Getenv(helper) == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestProjectShareCloseWaitsForTransportAndChild$", "-test.timeout=20s")
	cmd.Env = append(os.Environ(), helper+"=1")
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	reader := &observedProjectReader{ReadCloser: output, entered: make(chan struct{})}
	share := &projectShare{cmd: cmd, transport: projectTransport{ReadCloser: reader, WriteCloser: input}, done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		_ = share.transport.Close()
		t.Fatal(err)
	}
	served, waited := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	root := t.TempDir()
	go func() {
		defer close(share.done)
		_ = reversesshfs.ServeConfined(share.transport, root, false, nil)
		close(served)
		share.stop()
		_ = cmd.Wait()
		close(waited)
		<-release
	}()
	defer func() { finish(); share.close() }()
	awaitJobSignal(t, reader.entered)
	closed := make(chan struct{})
	go func() { share.close(); close(closed) }()
	awaitJobSignal(t, served)
	awaitJobSignal(t, waited)
	if cmd.ProcessState == nil {
		t.Fatal("shared child has not been reaped")
	}
	select {
	case <-closed:
		t.Fatal("close returned before the share worker finished cleanup")
	default:
	}
	finish()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("close did not return after transport and child exited")
	}
	share.close()
}
