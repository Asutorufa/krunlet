//go:build linux || darwin

package krunlet

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
)

func TestYuhaiinFlowProtocol(t *testing.T) {
	for _, tt := range []struct {
		transport byte
		source    string
		target    string
	}{
		{1, "192.168.127.2", "1.1.1.1"},
		{2, "fd42:6b72:756e::2", "2606:4700:4700::1111"},
	} {
		t.Run(tt.target, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "kny-")
			if err != nil { t.Fatal(err) }
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			path := filepath.Join(dir, "inbound.sock")
			l, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			accepted := make(chan []byte, 1)
			go func() {
				conn, e := l.Accept()
				if e != nil {
					accepted <- nil
					return
				}
				defer conn.Close()
				size := 18
				if tt.transport == 2 {
					size = 42
				}
				header := make([]byte, size)
				_, _ = io.ReadFull(conn, header)
				accepted <- header
				if tt.transport == 2 {
					var frame [2]byte
					_, _ = io.ReadFull(conn, frame[:])
					buf := make([]byte, int(binary.BigEndian.Uint16(frame[:])))
					_, _ = io.ReadFull(conn, buf)
					_, _ = conn.Write(append(frame[:], buf...))
				}
			}()
			src := net.ParseIP(tt.source)
			source := tcpip.AddrFromSlice(src.To4())
			target := netip.MustParseAddr(tt.target)
			if target.Is6() {
				source = tcpip.AddrFromSlice(src.To16())
			}
			c, err := dialYuhaiin(context.Background(), &YuhaiinConfig{Socket: path}, tt.transport, source, 12345, target, 443)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			select {
			case header := <-accepted:
				if len(header) == 0 || string(header[:4]) != "KRN1" || header[4] != tt.transport || binary.BigEndian.Uint16(header[8:10]) != 443 {
					t.Fatalf("invalid yuhaiin flow header: %x", header)
				}
			case <-time.After(time.Second * 3):
				t.Fatal("missing yuhaiin handshake")
			}
			if tt.transport == 2 {
				stream := &yuhaiinPacketConn{Conn: c}
				payload := []byte("test UDP")
				if n, e := stream.Write(payload); e != nil || n != len(payload) {
					t.Fatalf("write %d: %v", n, e)
				}
				buf := make([]byte, 64)
				_ = stream.SetReadDeadline(time.Now().Add(time.Second * 2))
				n, e := stream.Read(buf)
				if e != nil || !bytes.Equal(buf[:n], payload) {
					t.Fatalf("read %q: %v", buf[:n], e)
				}
			}
		})
	}
}

func TestYuhaiinRequiresTrustedSocket(t *testing.T) {
	f := filepath.Join(t.TempDir(), "ordinary")
	if err := os.WriteFile(f, []byte("not a socket"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"", f, filepath.Join(t.TempDir(), "missing")} {
		if _, err := normalizeYuhaiin(&YuhaiinConfig{Socket: s}); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
	root := t.TempDir()
	if _, err := New(Options{RootFS: root, Yuhaiin: &YuhaiinConfig{Socket: "/tmp/unused.sock"}}); err == nil {
		t.Fatal("yuhaiin must require explicit Network=true")
	}
}

func TestYuhaiinNoDirectFallback(t *testing.T) {
	src := tcpip.AddrFrom4Slice([]byte{192, 168, 127, 2})
	c, err := dialYuhaiin(context.Background(), &YuhaiinConfig{Socket: filepath.Join(t.TempDir(), "missing.sock")}, 1, src, 1010, netip.MustParseAddr("1.1.1.1"), 443)
	if err == nil {
		_ = c.Close()
		t.Fatal("missing yuhaiin inbound fell back to direct")
	}
}
