//go:build linux || darwin

package krunlet

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/services/dhcp"
	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// An isolated QEMU-framed Unix socket is the *only* virtio-net transport.
// Guest packets terminate at gVisor's TCP/UDP forwarders, which must consult
// the policy before opening ANY host socket. No TSI path may be enabled.
const (
	gatewayIP            = "192.168.127.1"
	gatewaySubnet        = "192.168.127.0/24"
	gatewayMAC           = "5a:94:ef:e4:0c:ee"
	guestMAC             = "5a:94:ef:e4:0c:ef"
	gatewayMTU           = 1500
	gatewayIPv6Address   = "fd42:6b72:756e::1"
	gatewayIPv6Prefix    = "fd42:6b72:756e::/64"
	gatewayIPv6LinkLocal = "fe80::1"
)

type networkLease struct {
	socket string
	close  func()
}

func (l *networkLease) Close() {
	if l != nil && l.close != nil {
		l.close()
	}
}

func prepareNetwork(ctx context.Context, policy *NetworkPolicy) (*networkLease, error) {
	if policy == nil {
		return &networkLease{}, nil
	}
	if _, err := compileNetworkPolicy(policy); err != nil {
		return nil, err
	}
	gw, err := newGuestNetwork(ctx, policy)
	if err != nil {
		return nil, err
	}
	// Short path to respect the Unix socket path length on macOS.
	dir, err := os.MkdirTemp("/tmp", "krunlet-net-")
	if err != nil {
		gw.Close()
		return nil, err
	}
	socket := filepath.Join(dir, "net.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		gw.Close()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := os.Chmod(socket, 0600); err != nil {
		_ = ln.Close()
		gw.Close()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	var mu sync.Mutex
	var active net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		mu.Lock()
		active = c
		mu.Unlock()
		defer c.Close()
		// QemuProtocol is a 4-byte big-endian Ethernet frame length.
		_ = gw.Accept(runCtx, c)
	}()
	var once sync.Once
	closeFn := func() {
		once.Do(func() {
			cancel()
			_ = ln.Close()
			mu.Lock()
			if active != nil {
				_ = active.Close()
			}
			mu.Unlock()
			<-done
			gw.Close()
			_ = os.RemoveAll(dir)
		})
	}
	return &networkLease{socket: socket, close: closeFn}, nil
}

type guestNetwork struct {
	wire       *guestWire
	link       *tap.LinkEndpoint
	dhcpServer *dhcp.Server
	stack      *stack.Stack
	ctx        context.Context
	cancel     context.CancelFunc
}

// Accept uses the QEMU network framing expected by libkrun's Unixstream
// virtio-net backend. We deliver multicast frames directly rather than using
// tap.Switch, whose single-peer CAM does not forward IPv6 multicast NDP.
func (g *guestNetwork) Accept(ctx context.Context, conn net.Conn) error {
	g.wire.setConn(conn)
	defer g.wire.setConn(nil)
	// Send a router advertisement now, in response to Router Solicitations,
	// and periodically for guests that miss the first advertisement.
	announceCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-announceCtx.Done():
				return
			case <-ticker.C:
				_ = g.wire.writeFrame(ipv6RouterAdvertisement())
			}
		}
	}()
	_ = g.wire.writeFrame(ipv6RouterAdvertisement())
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			return err
		}
		size := binary.BigEndian.Uint32(hdr[:])
		if size < 14 || size > 65535 {
			return fmt.Errorf("invalid guest Ethernet frame length: %d", size)
		}
		frame := make([]byte, int(size))
		if _, err := io.ReadFull(conn, frame); err != nil {
			return err
		}
		if !bytes.Equal(frame[6:12], guestMACBytes[:]) {
			// The guest NIC has a fixed MAC. Reject spoofed source frames.
			continue
		}
		proto := binary.BigEndian.Uint16(frame[12:14])
		if proto != 0x0800 && proto != 0x0806 && proto != 0x86dd {
			continue
		}
		// A Router Solicitation triggers an immediate SLAAC announcement.
		if isRouterSolicitation(frame) {
			_ = g.wire.writeFrame(ipv6RouterAdvertisement())
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(frame[14:]),
		})
		g.link.DeliverNetworkPacket(tcpip.NetworkProtocolNumber(proto), pkt)
		pkt.DecRef()
	}
}

// guestWire replaces the generic Ethernet switch. With a single guest it
// delivers both unicast and multicast (especially IPv6 NDP) to the VM.
type guestWire struct {
	mu   sync.Mutex
	conn net.Conn
}

var guestMACBytes = [6]byte{0x5a, 0x94, 0xef, 0xe4, 0x0c, 0xef}
var gatewayMACBytes = [6]byte{0x5a, 0x94, 0xef, 0xe4, 0x0c, 0xee}

func (w *guestWire) setConn(conn net.Conn) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.conn = conn
}

func (w *guestWire) DeliverNetworkPacket(_ tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	_ = w.writeFrame(pkt.ToView().AsSlice())
}

func (w *guestWire) writeFrame(frame []byte) error {
	if len(frame) < 14 || len(frame) > 65535 {
		return errors.New("invalid outgoing Ethernet frame")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn == nil {
		return errors.New("guest network is not connected")
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(frame)))
	if err := writeFull(w.conn, hdr[:]); err != nil {
		return err
	}
	return writeFull(w.conn, frame)
}

// writeFull preserves the 4-byte length prefix and entire Ethernet payload
// even if a stream writer returns a short successful write.
func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func isRouterSolicitation(frame []byte) bool {
	if len(frame) < 14+40+8 || binary.BigEndian.Uint16(frame[12:14]) != 0x86dd {
		return false
	}
	ip := frame[14:]
	return ip[0]>>4 == 6 && ip[6] == 58 && ip[7] == 255 && ip[40] == 133 && ip[41] == 0
}

// ipv6RouterAdvertisement advertises an isolated ULA /64 and a default
// route via fe80::1. Linux guests use SLAAC without DHCPv6 or host config.
func ipv6RouterAdvertisement() []byte {
	const icmpLen = 16 + 8 + 32
	frame := make([]byte, 14+40+icmpLen)
	copy(frame[0:6], []byte{0x33, 0x33, 0, 0, 0, 1})
	copy(frame[6:12], gatewayMACBytes[:])
	binary.BigEndian.PutUint16(frame[12:14], 0x86dd)
	ip := frame[14:54]
	ip[0] = 0x60
	binary.BigEndian.PutUint16(ip[4:6], icmpLen)
	ip[6], ip[7] = 58, 255 // ICMPv6, NDP hop limit 255
	src := netip.MustParseAddr(gatewayIPv6LinkLocal).As16()
	dst := netip.MustParseAddr("ff02::1").As16()
	copy(ip[8:24], src[:])
	copy(ip[24:40], dst[:])
	icmp := frame[54:]
	icmp[0], icmp[4] = 134, 64                  // RA, hop limit
	binary.BigEndian.PutUint16(icmp[6:8], 1800) // default router lifetime
	icmp[16], icmp[17] = 1, 1                   // source link-layer MAC option
	copy(icmp[18:24], gatewayMACBytes[:])
	icmp[24], icmp[25], icmp[26] = 3, 4, 64        // Prefix Information, 32 bytes, /64
	icmp[27] = 0xc0                                // on-link, autonomous address configuration
	binary.BigEndian.PutUint32(icmp[28:32], 86400) // valid lifetime
	binary.BigEndian.PutUint32(icmp[32:36], 14400) // preferred lifetime
	prefix := netip.MustParsePrefix(gatewayIPv6Prefix).Addr().As16()
	copy(icmp[40:56], prefix[:])
	// IPv6 pseudo-header: source, destination, payload length and NH=58.
	var pseudo [8]byte
	binary.BigEndian.PutUint32(pseudo[:4], icmpLen)
	pseudo[7] = 58
	binary.BigEndian.PutUint16(icmp[2:4], internetChecksum(src[:], dst[:], pseudo[:], icmp))
	return frame
}

func internetChecksum(chunks ...[]byte) uint16 {
	var sum uint32
	for _, b := range chunks {
		for len(b) >= 2 {
			sum += uint32(binary.BigEndian.Uint16(b[:2]))
			b = b[2:]
		}
		if len(b) != 0 {
			sum += uint32(b[0]) << 8
		}
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func (g *guestNetwork) Close() {
	g.cancel()
	// Close sockets and gVisor after the listener has stopped.
	g.stack.Close()
	g.stack.Wait()
}

func newGuestNetwork(ctx context.Context, policy *NetworkPolicy) (*guestNetwork, error) {
	config := &types.Configuration{
		MTU:               gatewayMTU,
		Subnet:            gatewaySubnet,
		GatewayIP:         gatewayIP,
		GatewayMacAddress: gatewayMAC,
	}
	_, subnet, err := net.ParseCIDR(config.Subnet)
	if err != nil {
		return nil, err
	}
	pool := tap.NewIPPool(subnet)
	pool.Reserve(net.ParseIP(gatewayIP), gatewayMAC)
	link, err := tap.NewLinkEndpoint(false, gatewayMTU, gatewayMAC, gatewayIP, nil)
	if err != nil {
		return nil, err
	}
	wire := &guestWire{}
	link.Connect(wire)
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4, icmp.NewProtocol6},
	})
	if e := s.CreateNIC(1, link); e != nil {
		return nil, fmt.Errorf("create gVisor NIC: %s", e)
	}
	addr := tcpip.AddrFrom4Slice(net.ParseIP(gatewayIP).To4())
	if e := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber, AddressWithPrefix: addr.WithPrefix(),
	}, stack.AddressProperties{}); e != nil {
		s.Close()
		return nil, fmt.Errorf("configure gVisor gateway: %s", e)
	}
	// Both the advertised ULA router address and link-local NDP address
	// belong to the gateway's NIC. The guest creates its address via SLAAC.
	for _, name := range []string{gatewayIPv6Address, gatewayIPv6LinkLocal} {
		parsed := netip.MustParseAddr(name).As16()
		if e := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
			Protocol: ipv6.ProtocolNumber,
			AddressWithPrefix: tcpip.AddressWithPrefix{
				Address: tcpip.AddrFrom16Slice(parsed[:]), PrefixLen: 64,
			},
		}, stack.AddressProperties{}); e != nil {
			s.Close()
			return nil, fmt.Errorf("configure IPv6 gateway address: %s", e)
		}
	}
	s.SetSpoofing(1, true)
	s.SetPromiscuousMode(1, true)
	dst, e := tcpip.NewSubnet(tcpip.AddrFromSlice(subnet.IP), tcpip.MaskFromBytes(subnet.Mask))
	if e != nil {
		s.Close()
		return nil, fmt.Errorf("configure route: %s", e)
	}
	_, subnet6, err := net.ParseCIDR(gatewayIPv6Prefix)
	if err != nil {
		s.Close()
		return nil, err
	}
	dst6, e := tcpip.NewSubnet(tcpip.AddrFromSlice(subnet6.IP.To16()), tcpip.MaskFromBytes(subnet6.Mask))
	if e != nil {
		s.Close()
		return nil, fmt.Errorf("configure IPv6 route: %s", e)
	}
	s.SetRouteTable([]tcpip.Route{
		{Destination: dst, NIC: 1},
		{Destination: dst6, NIC: 1},
	})
	server, err := dhcp.New(config, s, pool)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("configure DHCP: %w", err)
	}
	child, cancel := context.WithCancel(ctx)
	g := &guestNetwork{wire: wire, link: link, dhcpServer: server, stack: s, ctx: child, cancel: cancel}
	g.registerForwarders(policy)
	go func() { _ = server.Serve() }()
	return g, nil
}

func (g *guestNetwork) permitted(policy *NetworkPolicy, address tcpip.Address, port uint16, protocol string) (netip.Addr, bool) {
	ip, err := netip.ParseAddr(address.String())
	if err != nil || !ip.IsValid() || ip.Is4In6() || (!ip.Is4() && !ip.Is6()) {
		return netip.Addr{}, false
	}
	ok, err := policy.Allows(ip, port, protocol)
	return ip, err == nil && ok
}

func (g *guestNetwork) registerForwarders(policy *NetworkPolicy) {
	tcpFwd := tcp.NewForwarder(g.stack, 0, 128, func(req *tcp.ForwarderRequest) {
		ip, ok := g.permitted(policy, req.ID().LocalAddress, req.ID().LocalPort, "tcp")
		if !ok {
			req.Complete(true)
			return
		}
		go g.handleTCP(req, ip, req.ID().LocalPort)
	})
	g.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)
	udpFwd := udp.NewForwarder(g.stack, func(req *udp.ForwarderRequest) {
		ip, ok := g.permitted(policy, req.ID().LocalAddress, req.ID().LocalPort, "udp")
		if !ok {
			return
		}
		g.handleUDP(req, ip, req.ID().LocalPort)
	})
	g.stack.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)
}

// destinationNetwork selects the address family explicitly: an IPv6 policy
// decision cannot accidentally fall through to an IPv4 or DNS-backed dial.
func destinationNetwork(protocol string, ip netip.Addr) string {
	if ip.Is6() {
		return protocol + "6"
	}
	return protocol + "4"
}

func (g *guestNetwork) handleTCP(req *tcp.ForwarderRequest, ip netip.Addr, port uint16) {
	ctx, cancel := context.WithTimeout(g.ctx, 10*time.Second)
	defer cancel()
	outbound, err := (&net.Dialer{}).DialContext(ctx, destinationNetwork("tcp", ip), net.JoinHostPort(ip.String(), strconv.Itoa(int(port))))
	if err != nil {
		req.Complete(true)
		return
	}
	var wq waiter.Queue
	endpoint, tcpErr := req.CreateEndpoint(&wq)
	req.Complete(false)
	if tcpErr != nil {
		_ = outbound.Close()
		return
	}
	guest := gonet.NewTCPConn(&wq, endpoint)
	// Both directions are paired. On either EOF/error, terminate both sockets.
	go func() {
		defer guest.Close()
		defer outbound.Close()
		done := make(chan struct{}, 1)
		go func() { _, _ = io.Copy(outbound, guest); done <- struct{}{} }()
		go func() { _, _ = io.Copy(guest, outbound); done <- struct{}{} }()
		select {
		case <-done:
		case <-g.ctx.Done():
		}
	}()
}

func (g *guestNetwork) handleUDP(req *udp.ForwarderRequest, ip netip.Addr, port uint16) {
	var wq waiter.Queue
	endpoint, err := req.CreateEndpoint(&wq)
	if err != nil {
		return
	}
	guest := gonet.NewUDPConn(&wq, endpoint)
	go func() {
		defer guest.Close()
		outbound, err := (&net.Dialer{}).DialContext(g.ctx, destinationNetwork("udp", ip), net.JoinHostPort(ip.String(), strconv.Itoa(int(port))))
		if err != nil {
			return
		}
		defer outbound.Close()
		var wg sync.WaitGroup

		var peerMu sync.RWMutex
		var peer net.Addr
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 65535)
			for {
				_ = outbound.SetReadDeadline(time.Now().Add(60 * time.Second))
				n, e := outbound.Read(buf)
				if e != nil {
					return
				}
				peerMu.RLock()
				recipient := peer
				peerMu.RUnlock()
				if recipient != nil {
					if _, e = guest.WriteTo(buf[:n], recipient); e != nil {
						return
					}
				}
			}
		}()
		buf := make([]byte, 65535)
		for {
			select {
			case <-g.ctx.Done():
				return
			default:
			}
			_ = guest.SetReadDeadline(time.Now().Add(60 * time.Second))
			n, from, e := guest.ReadFrom(buf)
			if e != nil {
				break
			}
			peerMu.Lock()
			peer = from
			peerMu.Unlock()
			if _, e = outbound.Write(buf[:n]); e != nil {
				break
			}
		}

		_ = outbound.Close()
		wg.Wait()
	}()
}
