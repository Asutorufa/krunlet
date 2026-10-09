//go:build linux || darwin

package krunlet

import (
	"bytes"
	"context"
	"encoding/binary"
	"gvisor.dev/gvisor/pkg/tcpip"
	"io"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"
)

func TestGVisorGatewayLifecycle(t *testing.T) {
	policy := &NetworkPolicy{Mode: NetworkAllowlist, Allow: []NetworkRule{{CIDR: "1.1.1.1/32", Port: 443, Protocol: "tcp"}}}
	lease, err := prepareNetwork(context.Background(), policy)
	if err != nil {
		t.Fatal(err)
	}
	path := lease.socket
	if path == "" {
		t.Fatal("gateway did not create socket")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&077 != 0 {
		t.Fatalf("gateway socket permissions too permissive: %o", info.Mode().Perm())
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	lease.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket not cleaned up: %v", err)
	}
	lease.Close()
}

func TestGVisorIPv6RulesAndGateway(t *testing.T) {
	p := &NetworkPolicy{Mode: NetworkAllowlist, Allow: []NetworkRule{
		{CIDR: "2001:4860:4860::/48", Port: 443, Protocol: "tcp"},
		{CIDR: "192.0.2.0/24", Port: 53, Protocol: "udp"},
	}}
	g, err := newGuestNetwork(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	for _, tt := range []struct {
		addr     string
		port     uint16
		protocol string
		allow    bool
	}{
		{"2001:4860:4860::8888", 443, "tcp", true},
		{"2001:4860:4860::8888", 443, "udp", false},
		{"2001:4860:4860::8888", 53, "tcp", false},
		{"2001:4860:4861::8888", 443, "tcp", false},
		{"192.0.2.9", 53, "udp", true},
		{"::1", 443, "tcp", false},
	} {
		ip, ok := g.permitted(p, tcpip.AddrFromSlice(netip.MustParseAddr(tt.addr).AsSlice()), tt.port, tt.protocol)
		if ok != tt.allow || (ok && ip.String() != tt.addr) {
			t.Errorf("permitted(%s:%d/%s)=%t, ip=%s want %t", tt.addr, tt.port, tt.protocol, ok, ip, tt.allow)
		}
	}
	lease, err := prepareNetwork(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
}

func TestIPv6RouterAdvertisement(t *testing.T) {
	frame := ipv6RouterAdvertisement()
	if len(frame) != 14+40+56 {
		t.Fatalf("RA length %d", len(frame))
	}
	if !bytes.Equal(frame[:6], []byte{0x33, 0x33, 0, 0, 0, 1}) {
		t.Fatalf("wrong multicast MAC: %x", frame[:6])
	}
	if binary.BigEndian.Uint16(frame[12:14]) != 0x86dd {
		t.Fatal("wrong ethernet protocol")
	}
	ip := frame[14:54]
	icmp := frame[54:]
	if ip[6] != 58 || ip[7] != 255 || icmp[0] != 134 {
		t.Fatal("invalid ICMPv6 RA")
	}
	if ip[0]>>4 != 6 || binary.BigEndian.Uint16(ip[4:6]) != 56 {
		t.Fatal("invalid IPv6 packet")
	}
	if binary.BigEndian.Uint16(icmp[6:8]) == 0 {
		t.Fatal("missing default router lifetime")
	}
	if icmp[24] != 3 || icmp[25] != 4 || icmp[26] != 64 || icmp[27] != 0xc0 {
		t.Fatalf("SLAAC prefix option invalid: %x", icmp[24:28])
	}
	prefix := netip.MustParsePrefix(gatewayIPv6Prefix).Addr().As16()
	if !bytes.Equal(icmp[40:56], prefix[:]) {
		t.Fatal("wrong SLAAC prefix")
	}
	var pseudo [8]byte
	binary.BigEndian.PutUint32(pseudo[:4], uint32(len(icmp)))
	pseudo[7] = 58
	if checksum := internetChecksum(ip[8:24], ip[24:40], pseudo[:], icmp); checksum != 0 {
		t.Fatalf("invalid ICMPv6 checksum: %04x", checksum)
	}
	if isRouterSolicitation(frame) {
		t.Fatal("RA mistaken for RS")
	}
}

func TestGVisorQemuIPv6RouterSolicitation(t *testing.T) {
	p := &NetworkPolicy{Mode: NetworkAllowlist}
	g, err := newGuestNetwork(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	vm, gateway := net.Pipe()
	defer vm.Close()
	defer gateway.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.Accept(ctx, gateway) }()
	// The first frame is a multicast RA that enables the guest's SLAAC.
	readFrame := func() []byte {
		t.Helper()
		_ = vm.SetReadDeadline(time.Now().Add(2 * time.Second))
		var hdr [4]byte
		if _, err := io.ReadFull(vm, hdr[:]); err != nil {
			t.Fatal(err)
		}
		size := binary.BigEndian.Uint32(hdr[:])
		if size > 65535 || size < 14 {
			t.Fatalf("invalid frame size %d", size)
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(vm, buf); err != nil {
			t.Fatal(err)
		}
		return buf
	}
	if got := readFrame(); len(got) < 55 || got[54] != 134 {
		t.Fatal("missing initial RA")
	}
	// Send an IPv6 Router Solicitation from the configured guest MAC.
	rs := make([]byte, 14+40+8)
	copy(rs[0:6], gatewayMACBytes[:])
	copy(rs[6:12], guestMACBytes[:])
	binary.BigEndian.PutUint16(rs[12:14], 0x86dd)
	rs[14] = 0x60
	binary.BigEndian.PutUint16(rs[14+4:14+6], 8)
	rs[14+6], rs[14+7] = 58, 255
	rs[14+40] = 133
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(rs)))
	if _, err := vm.Write(append(hdr, rs...)); err != nil {
		t.Fatal(err)
	}
	if got := readFrame(); len(got) < 55 || got[54] != 134 {
		t.Fatal("missing solicited RA")
	}
	cancel()
	_ = vm.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not exit")
	}
}

func TestDestinationNetworkIPv4IPv6(t *testing.T) {
	for _, tt := range []struct{ proto, ip, want string }{
		{"tcp", "1.2.3.4", "tcp4"},
		{"udp", "1.2.3.4", "udp4"},
		{"tcp", "2606:4700::1111", "tcp6"},
		{"udp", "2001:db8::1", "udp6"},
	} {
		if got := destinationNetwork(tt.proto, netip.MustParseAddr(tt.ip)); got != tt.want {
			t.Errorf("destinationNetwork(%s,%s) = %q; want %q", tt.proto, tt.ip, got, tt.want)
		}
	}
}

type shortWriteBuffer struct {
	buf bytes.Buffer
}

func (w *shortWriteBuffer) Write(p []byte) (int, error) {
	if len(p) > 2 {
		p = p[:2]
	}
	return w.buf.Write(p)
}

func TestWriteFullHandlesShortWrites(t *testing.T) {
	w := &shortWriteBuffer{}
	message := []byte("123456789abcde")
	if err := writeFull(w, message); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.buf.Bytes(), message) {
		t.Fatalf("short write truncated: %q", w.buf.Bytes())
	}
}
