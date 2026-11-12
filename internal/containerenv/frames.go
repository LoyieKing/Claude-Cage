package containerenv

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
)

// guestSourceMAC is parsed once. Every frame still compares the Ethernet
// source; the comparison itself no longer allocates.
var guestSourceMAC = func() net.HardwareAddr {
	mac, err := net.ParseMAC(GuestMAC)
	if err != nil {
		panic(err)
	}
	return append(net.HardwareAddr(nil), mac...)
}()

// A compromised guest can replace its helper. Validate framing again before
// the shared netstack allocates packets or indexes the Ethernet header.
type boundedFrameConnection struct {
	net.Conn
	pending []byte
	buffer  []byte
	mac     net.HardwareAddr
}

func newBoundedFrameConnection(conn net.Conn) *boundedFrameConnection {
	return &boundedFrameConnection{Conn: conn, buffer: make([]byte, 4+maxFrameSize), mac: guestSourceMAC}
}

func (c *boundedFrameConnection) Read(output []byte) (int, error) {
	if len(output) == 0 {
		return 0, nil
	}
	if len(c.pending) == 0 {
		// pending aliases c.buffer and is empty here, so the next frame can
		// reuse the same validation buffer.
		frame, err := readFrame(c.Conn, c.buffer[4:4+maxFrameSize])
		if err != nil {
			return 0, err
		}
		if len(frame) < 12 || !bytes.Equal(frame[6:12], c.mac) {
			return 0, errors.New("unexpected container Ethernet source")
		}
		binary.BigEndian.PutUint32(c.buffer[:4], uint32(len(frame)))
		c.pending = c.buffer[:4+len(frame)]
	}
	n := copy(output, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

// A TAP read returns exactly one Ethernet frame. Jumbo frames are disabled;
// the small allowance includes Ethernet/VLAN headers above the configured MTU.
const maxFrameSize = NetworkMTU + 64

var frameBufferPool = sync.Pool{New: func() any {
	buffer := make([]byte, 4+maxFrameSize)
	return &buffer
}}

func writeFrame(writer io.Writer, frame []byte) error {
	if len(frame) < 14 || len(frame) > maxFrameSize {
		return errors.New("invalid container Ethernet frame length")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(frame)))
	return writeGather(writer, header[:], frame)
}

// writeGather copies the header and payload once and writes them through the
// writer. os.File.Write stays on the runtime poller, so Close and deadlines
// unblock a full pipe. A raw writev on File.Fd would drop that contract.
func writeGather(writer io.Writer, header, payload []byte) error {
	return writeCombined(writer, header, payload)
}

func readFrame(reader io.Reader, buffer []byte) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size < 14 || size > maxFrameSize || int(size) > len(buffer) {
		return nil, errors.New("invalid container Ethernet frame length")
	}
	frame := buffer[:int(size)]
	_, err := io.ReadFull(reader, frame)
	return frame, err
}

func writeCombined(writer io.Writer, header, payload []byte) error {
	boxed := frameBufferPool.Get().(*[]byte)
	buffer := *boxed
	if cap(buffer) < 4+len(payload) {
		buffer = make([]byte, 4+maxFrameSize)
	}
	n := copy(buffer, header)
	n += copy(buffer[n:], payload)
	err := writeAll(writer, buffer[:n])
	*boxed = buffer
	frameBufferPool.Put(boxed)
	return err
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if n < 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
