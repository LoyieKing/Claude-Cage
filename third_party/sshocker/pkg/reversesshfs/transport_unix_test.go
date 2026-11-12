//go:build linux || darwin

package reversesshfs

import (
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestBuiltinTransportProgressesDuringBidirectionalSaturation(t *testing.T) {
	const size = 2 << 20
	const helper = "CAGE_TEST_SATURATED_DUPLEX"
	if os.Getenv(helper) == "1" {
		// SSH makes stdin nonblocking. With a shared socket description that
		// also makes stdout nonblocking, so output cannot trap its event loop.
		if err := unix.SetNonblock(0, true); err != nil {
			os.Exit(2)
		}
		pending := bytes.Repeat([]byte("x"), size)
		buffer := make([]byte, 32<<10)
		received := 0
		for len(pending) > 0 || received < size {
			fds := []unix.PollFd{{Fd: 0}, {Fd: 1}}
			if received < size {
				fds[0].Events = unix.POLLIN
			}
			if len(pending) > 0 {
				fds[1].Events = unix.POLLOUT
			}
			if _, err := unix.Poll(fds, 5000); err != nil {
				os.Exit(3)
			}
			if fds[1].Revents&unix.POLLOUT != 0 {
				n, err := unix.Write(1, pending)
				if n > 0 {
					pending = pending[n:]
				}
				if err != nil && err != unix.EAGAIN && err != unix.EINTR {
					os.Exit(4)
				}
			}
			if fds[0].Revents&unix.POLLIN != 0 {
				n, err := unix.Read(0, buffer)
				if n > 0 {
					for _, value := range buffer[:n] {
						if value != 'y' {
							os.Exit(5)
						}
					}
					received += n
				}
				if err != nil && err != unix.EAGAIN && err != unix.EINTR {
					os.Exit(6)
				}
			}
		}
		os.Exit(0)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestBuiltinTransportProgressesDuringBidirectionalSaturation$")
	command.Env = append(os.Environ(), helper+"=1")
	stream, release, err := prepareBuiltinTransport(command)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := command.Start(); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	t.Cleanup(func() { stream.Close(); _ = command.Process.Kill(); _ = command.Wait() })
	if connection, ok := stream.(net.Conn); ok {
		_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	}
	written := make(chan error, 1)
	go func() {
		_, err := io.Copy(stream, bytes.NewReader(bytes.Repeat([]byte("y"), size)))
		written <- err
	}()
	output := make([]byte, size)
	if _, err := io.ReadFull(stream, output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output, bytes.Repeat([]byte("x"), size)) {
		t.Fatal("duplex transfer corrupted output")
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
}
