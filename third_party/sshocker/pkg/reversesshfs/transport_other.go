//go:build !linux && !darwin

package reversesshfs

import (
	"io"
	"os/exec"

	"github.com/lima-vm/sshocker/pkg/util"
)

// Native Windows SSH standard handles are not Unix socket descriptors.
// Preserve that platform's process transport; Unix hosts use a socketpair.
func prepareBuiltinTransport(command *exec.Cmd) (io.ReadWriteCloser, func(), error) {
	input, err := command.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		_ = input.Close()
		return nil, nil, err
	}
	return &util.RWC{ReadCloser: output, WriteCloser: input}, func() {}, nil
}
