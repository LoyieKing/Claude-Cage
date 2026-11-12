package containerenv

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/miekg/dns"
	"github.com/sylvinus/agent-vm/internal/cage"
)

func TestFramesRejectInvalidLengthsAndTruncation(t *testing.T) {
	for _, size := range []uint32{0, 13, maxFrameSize + 1, 0xffffffff} {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], size)
		if _, err := readFrame(bytes.NewReader(header[:]), make([]byte, maxFrameSize)); err == nil {
			t.Fatalf("accepted frame size %d", size)
		}
	}
	var stream bytes.Buffer
	frame := bytes.Repeat([]byte{1}, 64)
	if err := writeFrame(&stream, frame); err != nil {
		t.Fatal(err)
	}
	if _, err := readFrame(bytes.NewReader(stream.Bytes()[:stream.Len()-1]), make([]byte, maxFrameSize)); err != io.ErrUnexpectedEOF {
		t.Fatalf("truncated frame: %v", err)
	}
}

func TestContainerNetworkARPAndClosedDNS(t *testing.T) {
	network, err := NewNetwork(cage.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host, guest := net.Pipe()
	defer guest.Close()
	done := make(chan error, 1)
	go func() { done <- network.Run(ctx, host) }()
	defer func() {
		cancel()
		guest.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("network did not stop")
		}
	}()
	_ = guest.SetDeadline(time.Now().Add(3 * time.Second))
	guestMAC, _ := net.ParseMAC(GuestMAC)
	gatewayMAC, _ := net.ParseMAC(GatewayMAC)
	arp := &layers.ARP{AddrType: layers.LinkTypeEthernet, Protocol: layers.EthernetTypeIPv4, HwAddressSize: 6, ProtAddressSize: 4, Operation: layers.ARPRequest, SourceHwAddress: guestMAC, SourceProtAddress: net.ParseIP(GuestIP).To4(), DstHwAddress: make([]byte, 6), DstProtAddress: net.ParseIP(GatewayIP).To4()}
	ethernet := &layers.Ethernet{SrcMAC: guestMAC, DstMAC: net.HardwareAddr{255, 255, 255, 255, 255, 255}, EthernetType: layers.EthernetTypeARP}
	buffer := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{FixLengths: true}, ethernet, arp); err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(guest, buffer.Bytes()); err != nil {
		t.Fatal(err)
	}
	response, err := readFrame(guest, make([]byte, maxFrameSize))
	if err != nil {
		t.Fatal(err)
	}
	packet := gopacket.NewPacket(response, layers.LayerTypeEthernet, gopacket.Default)
	answer, ok := packet.Layer(layers.LayerTypeARP).(*layers.ARP)
	if !ok || answer.Operation != layers.ARPReply || !bytes.Equal(answer.SourceHwAddress, gatewayMAC) {
		t.Fatal("virtual gateway did not answer ARP")
	}

	query := new(dns.Msg)
	query.SetQuestion("example.com.", dns.TypeA)
	payload, _ := query.Pack()
	ip := &layers.IPv4{Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolUDP, SrcIP: net.ParseIP(GuestIP), DstIP: net.ParseIP(GatewayIP)}
	udp := &layers.UDP{SrcPort: 41000, DstPort: 53}
	_ = udp.SetNetworkLayerForChecksum(ip)
	ethernet = &layers.Ethernet{SrcMAC: guestMAC, DstMAC: gatewayMAC, EthernetType: layers.EthernetTypeIPv4}
	buffer = gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, ethernet, ip, udp, gopacket.Payload(payload)); err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(guest, buffer.Bytes()); err != nil {
		t.Fatal(err)
	}
	response, err = readFrame(guest, make([]byte, maxFrameSize))
	if err != nil {
		t.Fatal(err)
	}
	packet = gopacket.NewPacket(response, layers.LayerTypeEthernet, gopacket.Default)
	dnsLayer := packet.Layer(layers.LayerTypeDNS)
	if dnsLayer == nil {
		t.Fatalf("expected DNS refusal, got %v", packet)
	}
	if network.Gate.Status().Counters.DNSDenied == 0 {
		t.Fatal("DNS bypassed the closed host gate")
	}
}

func TestHostRejectsGuestMalformedFrames(t *testing.T) {
	for _, size := range []uint32{0, 13, 0xffffffff} {
		network, err := NewNetwork(cage.DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		host, guest := net.Pipe()
		done := make(chan error, 1)
		go func() { done <- network.Run(context.Background(), host) }()
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], size)
		_, _ = guest.Write(header[:])
		guest.Close()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("accepted size %d", size)
			}
		case <-time.After(time.Second):
			t.Fatal("malformed guest frame blocked host")
		}
	}
}

func TestProcessEndpointClosesIdleProcess(t *testing.T) {
	if os.Getenv("CAGE_CONTAINER_PIPE_TEST") == "1" {
		_, _ = io.Copy(os.Stdout, os.Stdin)
		os.Exit(0)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestProcessEndpointClosesIdleProcess$")
	command.Env = append(os.Environ(), "CAGE_CONTAINER_PIPE_TEST=1")
	endpoint, wait, err := ProcessEndpoint(command)
	if err != nil {
		t.Fatal(err)
	}
	_ = endpoint.SetDeadline(time.Now().Add(3 * time.Second))
	done := make(chan error, 1)
	go func() { _, err := endpoint.Write([]byte("frame")); done <- err }()
	got := make([]byte, 5)
	if _, err := io.ReadFull(endpoint, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "frame" {
		t.Fatalf("pipe payload %q", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	endpoint.Close()
	waited := make(chan struct{})
	go func() { _ = wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("idle helper was not reaped")
	}
}

func TestFrameShortWritesPartialReadsAndConcurrentClose(t *testing.T) {
	frame := bytes.Repeat([]byte{7}, 40)
	var short oneByteWriter
	if err := writeFrame(&short, frame); err != nil {
		t.Fatal(err)
	}
	if short.writes < 2 || !bytes.Equal(short.buf.Bytes()[4:], frame) || binary.BigEndian.Uint32(short.buf.Bytes()[:4]) != uint32(len(frame)) {
		t.Fatalf("short writes were not reassembled: writes=%d %x", short.writes, short.buf.Bytes())
	}
	var failing failAfterByte
	if err := writeFrame(&failing, frame); err == nil {
		t.Fatal("frame write succeeded after a short write error")
	}
	file, err := os.CreateTemp(t.TempDir(), "frame")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := writeFrame(file, frame); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(file)
	if err != nil || !bytes.Equal(stored[4:], frame) {
		t.Fatalf("vectored frame was not stored: %v %x", err, stored)
	}
	var reader oneByteReader
	reader.rest = append([]byte(nil), stored...)
	got, err := readFrame(&reader, make([]byte, maxFrameSize))
	if err != nil || !bytes.Equal(got, frame) {
		t.Fatalf("partial read: %v %x", err, got)
	}

	left, right := net.Pipe()
	defer right.Close()
	conn := newBoundedFrameConnection(left)
	good := bytes.Repeat([]byte{0}, 32)
	copy(good[6:12], guestSourceMAC)
	go func() {
		_ = writeFrame(right, good)
		_ = writeFrame(right, good)
	}()
	collect := func() []byte {
		t.Helper()
		var assembled []byte
		buf := make([]byte, 1)
		for len(assembled) < 4+len(good) {
			n, err := conn.Read(buf)
			assembled = append(assembled, buf[:n]...)
			if err != nil {
				t.Fatal(err)
			}
		}
		return assembled
	}
	first, second := collect(), collect()
	if !bytes.Equal(first[4:], good) || !bytes.Equal(second[4:], good) {
		t.Fatal("reused frame buffer corrupted a later frame")
	}
	bad := bytes.Repeat([]byte{1}, 32)
	go func() { _ = writeFrame(right, bad) }()
	if _, err := conn.Read(make([]byte, 16)); err == nil {
		t.Fatal("unexpected Ethernet source was accepted")
	}

	blocked, other := net.Pipe()
	defer other.Close()
	waiting := newBoundedFrameConnection(blocked)
	readDone := make(chan error, 1)
	go func() {
		_, err := waiting.Read(make([]byte, 8))
		readDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := waiting.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("close did not unblock a frame read")
		}
	case <-time.After(time.Second):
		t.Fatal("frame close blocked")
	}
}

func TestProcessEndpointCloseUnblocksRead(t *testing.T) {
	if os.Getenv("CAGE_CONTAINER_PIPE_TEST") == "close" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestProcessEndpointCloseUnblocksRead$")
	command.Env = append(os.Environ(), "CAGE_CONTAINER_PIPE_TEST=close")
	endpoint, wait, err := ProcessEndpoint(command)
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := endpoint.Read(make([]byte, 8))
		readDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("close did not unblock the process pipe")
		}
	case <-time.After(time.Second):
		t.Fatal("process close blocked")
	}
	waited := make(chan struct{})
	go func() { _ = wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("closed helper was not reaped")
	}
}

func TestRestartedNetworkGateIsClosedWithoutLease(t *testing.T) {
	first, err := NewNetwork(cage.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewNetwork(cage.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Gate.Close("test")
	defer second.Gate.Close("test")
	before, after := first.Gate.Status(), second.Gate.Status()
	if before.RunID == "" || before.RunID == after.RunID || before.Mode != cage.GateClosed || after.Mode != cage.GateClosed || before.Report != nil || after.Report != nil || !before.Until.IsZero() || !after.Until.IsZero() {
		t.Fatalf("restart reused a gate lease: %+v %+v", before, after)
	}
}

func TestBlockedFrameWriteReleasedByCloseAndDeadline(t *testing.T) {
	for _, name := range []string{"close", "deadline"} {
		t.Run(name, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			if err := writer.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			frame := bytes.Repeat([]byte{9}, 1024)
			for {
				err = writeFrame(writer, frame)
				if err != nil {
					break
				}
			}
			if err := writer.SetWriteDeadline(time.Time{}); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- writeFrame(writer, frame) }()
			time.Sleep(50 * time.Millisecond)
			if name == "close" {
				_ = writer.Close()
			} else if err := writer.SetWriteDeadline(time.Now()); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("blocked frame write succeeded after release")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("blocked frame write was not released")
			}
		})
	}
}

type oneByteWriter struct {
	buf    bytes.Buffer
	writes int
}

func (w *oneByteWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	w.writes++
	return w.buf.Write(data[:1])
}

type failAfterByte struct{ wrote bool }

func (w *failAfterByte) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if w.wrote {
		return 0, io.ErrClosedPipe
	}
	w.wrote = true
	return 1, nil
}

type oneByteReader struct{ rest []byte }

func (r *oneByteReader) Read(data []byte) (int, error) {
	if len(r.rest) == 0 {
		return 0, io.EOF
	}
	if len(data) == 0 {
		return 0, nil
	}
	data[0] = r.rest[0]
	r.rest = r.rest[1:]
	return 1, nil
}
