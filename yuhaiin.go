package krunlet

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
)

// YuhaiinConfig selects the native "krunlet" inbound hosted by yuhaiin.
// The unix stream transports each guest TCP connection and UDP flow after
// gVisor decodes the guest's network packets. No SOCKS/HTTP proxy is used.
type YuhaiinConfig struct {
	Socket string
}

func normalizeYuhaiin(cfg *YuhaiinConfig) (*YuhaiinConfig, error) {
	if cfg == nil {
		return nil, nil
	}
	if cfg.Socket == "" {
		return nil, errors.New("socket path is required")
	}
	full, err := filepath.Abs(cfg.Socket)
	if err != nil {
		return nil, err
	}
	// An arbitrary symlink could redirect trusted traffic to another
	// process, so require an actual Unix-domain socket at the path.
	info, err := os.Lstat(full)
	if err != nil {
		return nil, fmt.Errorf("yuhaiin socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("yuhaiin socket must be a Unix-domain socket, not a symlink or file")
	}
	return &YuhaiinConfig{Socket: full}, nil
}

// Wire format, version 1, per connection:
// 4 bytes "KRN1", 1-byte transport (TCP=1, UDP=2), 1-byte address family
// (4 or 6), src port and dst port (2 bytes each), src and dst IP of that
// family. TCP payload follows unframed. UDP datagrams are uint16 BE length
// followed by payload. This private local Unix protocol does not expose
// either the helper process or the guest to host TCP listeners.
func dialYuhaiin(ctx context.Context, cfg *YuhaiinConfig, protocol byte, source tcpip.Address, srcPort uint16, dst netip.Addr, dstPort uint16) (net.Conn, error) {
	if cfg == nil || cfg.Socket == "" {
		return nil, errors.New("missing yuhaiin inbound socket")
	}
	if protocol != 1 && protocol != 2 {
		return nil, errors.New("unsupported yuhaiin transport")
	}
	src, err := netip.AddrFromSlice(source.AsSlice())
	if err != nil || src.Is4() != dst.Is4() || src.Is6() != dst.Is6() || dstPort == 0 {
		return nil, errors.New("invalid IP family or destination in yuhaiin connection")
	}
	af := byte(4)
	if src.Is6() { af = 6 }
	header := make([]byte, 10, 42)
	copy(header, "KRN1")
	header[4], header[5] = protocol, af
	binary.BigEndian.PutUint16(header[6:8], srcPort)
	binary.BigEndian.PutUint16(header[8:10], dstPort)
	header = append(header, src.AsSlice()...)
	header = append(header, dst.AsSlice()...)
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", cfg.Socket)
	if err != nil { return nil, fmt.Errorf("connect yuhaiin inbound: %w", err) }
	if deadline, ok := ctx.Deadline(); ok { _ = conn.SetDeadline(deadline) }
	if err := writeYuhaiinFull(conn, header); err != nil { _ = conn.Close(); return nil, fmt.Errorf("yuhaiin inbound handshake: %w", err) }
	// No long-term I/O deadline: TLS, TCP and UDP flow lifetimes may exceed
	// the connection setup timeout. The gateway closes it with the VM.
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// yuhaiinPacketConn preserves UDP datagram boundaries across Unix streams.
type yuhaiinPacketConn struct { net.Conn }

func (c *yuhaiinPacketConn) Read(b []byte) (int, error) {
	var header [2]byte
	if _, err := io.ReadFull(c.Conn, header[:]); err != nil { return 0, err }
	n := int(binary.BigEndian.Uint16(header[:]))
	if n == 0 || n > len(b) {
		// Discard the full rejected payload to keep the framing aligned.
		_, _ = io.CopyN(io.Discard, c.Conn, int64(n))
		return 0, io.ErrShortBuffer
	}
	return io.ReadFull(c.Conn, b[:n])
}

func (c *yuhaiinPacketConn) Write(data []byte) (int, error) {
	if len(data) == 0 || len(data) > 65535 {
		return 0, fmt.Errorf("invalid UDP datagram length: %d", len(data))
	}
	var header [2]byte
	binary.BigEndian.PutUint16(header[:], uint16(len(data)))
	if err := writeYuhaiinFull(c.Conn, header[:]); err != nil { return 0, err }
	if err := writeYuhaiinFull(c.Conn, data); err != nil { return 0, err }
	return len(data), nil
}

func writeYuhaiinFull(w io.Writer, b []byte) error {
	for len(b)>0 {
		n,err:=w.Write(b)
		if err!=nil {return err}
		if n<=0 {return io.ErrShortWrite}
		b=b[n:]
	}
	return nil
}
