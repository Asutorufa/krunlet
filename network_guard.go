//go:build linux || darwin

package krunlet

import (
 "context"
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
 "gvisor.dev/gvisor/pkg/tcpip"
 "gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
 "gvisor.dev/gvisor/pkg/tcpip/network/arp"
 "gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
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
 gatewayIP = "192.168.127.1"
 gatewaySubnet = "192.168.127.0/24"
 gatewayMAC = "5a:94:ef:e4:0c:ee"
 guestMAC = "5a:94:ef:e4:0c:ef"
 gatewayMTU = 1500
)

type networkLease struct {
 socket string
 close func()
}

func (l *networkLease) Close() {
 if l != nil && l.close != nil { l.close() }
}

func prepareNetwork(ctx context.Context, policy *NetworkPolicy) (*networkLease, error) {
 if policy == nil { return &networkLease{}, nil }
 compiled, err := compileNetworkPolicy(policy)
 if err != nil { return nil, err }
 for _, r := range append(append([]compiledRule{}, compiled.allow...), compiled.block...) {
  if r.prefix.Addr().Is6() { return nil, errors.New("gVisor network backend is IPv4-only; IPv6 destination rules are unsupported") }
 }
 gw, err := newGuestNetwork(ctx, policy)
 if err != nil { return nil, err }
 // Short path to respect the Unix socket path length on macOS.
 dir, err := os.MkdirTemp("/tmp", "krunlet-net-")
 if err != nil { gw.Close(); return nil, err }
 socket := filepath.Join(dir, "net.sock")
 ln, err := net.Listen("unix", socket)
 if err != nil { gw.Close(); _ = os.RemoveAll(dir); return nil, err }
 if err := os.Chmod(socket, 0600); err != nil {
  _ = ln.Close(); gw.Close(); _ = os.RemoveAll(dir); return nil, err
 }
 runCtx, cancel := context.WithCancel(ctx)
 var mu sync.Mutex
 var active net.Conn
 done := make(chan struct{})
 go func() {
  defer close(done)
  c, err := ln.Accept()
  if err != nil { return }
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
   if active != nil { _ = active.Close() }
   mu.Unlock()
   <-done
   gw.Close()
   _ = os.RemoveAll(dir)
  })
 }
 return &networkLease{socket: socket, close: closeFn}, nil
}

type guestNetwork struct {
 switcher *tap.Switch
 dhcpServer *dhcp.Server
 stack *stack.Stack
 ctx context.Context
 cancel context.CancelFunc
}

func (g *guestNetwork) Accept(ctx context.Context, conn net.Conn) error {
 return g.switcher.Accept(ctx, conn, types.QemuProtocol)
}

func (g *guestNetwork) Close() {
 g.cancel()
 // Close sockets and gVisor after the listener has stopped.
 g.stack.Close()
 g.stack.Wait()
}

func newGuestNetwork(ctx context.Context, policy *NetworkPolicy) (*guestNetwork, error) {
 config := &types.Configuration{
  MTU: gatewayMTU,
  Subnet: gatewaySubnet,
  GatewayIP: gatewayIP,
  GatewayMacAddress: gatewayMAC,
 }
 _, subnet, err := net.ParseCIDR(config.Subnet)
 if err != nil { return nil, err }
 pool := tap.NewIPPool(subnet)
 pool.Reserve(net.ParseIP(gatewayIP), gatewayMAC)
 link, err := tap.NewLinkEndpoint(false, gatewayMTU, gatewayMAC, gatewayIP, nil)
 if err != nil { return nil, err }
 switcher := tap.NewSwitch(false, gatewayMTU)
 link.Connect(switcher)
 switcher.Connect(link)
 s := stack.New(stack.Options{
  NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
  TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
 })
 if e := s.CreateNIC(1, link); e != nil { return nil, fmt.Errorf("create gVisor NIC: %s", e) }
 addr := tcpip.AddrFrom4Slice(net.ParseIP(gatewayIP).To4())
 if e := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
  Protocol: ipv4.ProtocolNumber, AddressWithPrefix: addr.WithPrefix(),
 }, stack.AddressProperties{}); e != nil {
  s.Close()
  return nil, fmt.Errorf("configure gVisor gateway: %s", e)
 }
 s.SetSpoofing(1, true)
 s.SetPromiscuousMode(1, true)
 dst, e := tcpip.NewSubnet(tcpip.AddrFromSlice(subnet.IP), tcpip.MaskFromBytes(subnet.Mask))
 if e != nil { s.Close(); return nil, fmt.Errorf("configure route: %s", e) }
 s.SetRouteTable([]tcpip.Route{{Destination: dst, NIC: 1}})
 server, err := dhcp.New(config, s, pool)
 if err != nil { s.Close(); return nil, fmt.Errorf("configure DHCP: %w", err) }
 child, cancel := context.WithCancel(ctx)
 g := &guestNetwork{switcher: switcher, dhcpServer: server, stack:s, ctx:child, cancel:cancel}
 g.registerForwarders(policy)
 go func() { _ = server.Serve() }()
 return g,nil
}

func (g *guestNetwork) permitted(policy *NetworkPolicy, address tcpip.Address, port uint16, protocol string) (netip.Addr,bool) {
 ip, err := netip.ParseAddr(address.String())
 if err != nil || !ip.Is4() { return netip.Addr{},false }
 ok, err := policy.Allows(ip, port, protocol)
 return ip, err == nil && ok
}

func (g *guestNetwork) registerForwarders(policy *NetworkPolicy) {
 tcpFwd := tcp.NewForwarder(g.stack, 0, 128, func(req *tcp.ForwarderRequest) {
  ip, ok := g.permitted(policy, req.ID().LocalAddress, req.ID().LocalPort, "tcp")
  if !ok { req.Complete(true); return }
  go g.handleTCP(req, ip, req.ID().LocalPort)
 })
 g.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)
 udpFwd := udp.NewForwarder(g.stack, func(req *udp.ForwarderRequest) {
  ip, ok := g.permitted(policy, req.ID().LocalAddress, req.ID().LocalPort, "udp")
  if !ok { return }
  g.handleUDP(req, ip, req.ID().LocalPort)
 })
 g.stack.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)
}

func (g *guestNetwork) handleTCP(req *tcp.ForwarderRequest, ip netip.Addr, port uint16) {
 ctx, cancel := context.WithTimeout(g.ctx, 10*time.Second)
 defer cancel()
 outbound, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(ip.String(),strconv.Itoa(int(port))))
 if err != nil { req.Complete(true); return }
 var wq waiter.Queue
 endpoint, tcpErr := req.CreateEndpoint(&wq)
 req.Complete(false)
 if tcpErr != nil { _=outbound.Close(); return }
 guest := gonet.NewTCPConn(&wq, endpoint)
 // Both directions are paired. On either EOF/error, terminate both sockets.
 go func() {
  defer guest.Close()
  defer outbound.Close()
  done:=make(chan struct{},1)
  go func() { _,_ = io.Copy(outbound, guest); done<-struct{}{} }()
  go func() { _,_ = io.Copy(guest, outbound); done<-struct{}{} }()
  select { case <-done: case <-g.ctx.Done(): }
 }()
}

func (g *guestNetwork) handleUDP(req *udp.ForwarderRequest, ip netip.Addr, port uint16) {
 var wq waiter.Queue
 endpoint, err := req.CreateEndpoint(&wq)
 if err != nil { return }
 guest := gonet.NewUDPConn(&wq,endpoint)
 go func() {
  defer guest.Close()
  outbound, err := (&net.Dialer{}).DialContext(g.ctx, "udp", net.JoinHostPort(ip.String(),strconv.Itoa(int(port))))
  if err != nil { return }
  defer outbound.Close()
  var wg sync.WaitGroup
  done:=make(chan struct{})
  var peerMu sync.RWMutex
  var peer net.Addr
  wg.Add(1)
  go func(){
   defer wg.Done()
   buf:=make([]byte,65535)
   for {
    _ = outbound.SetReadDeadline(time.Now().Add(60*time.Second))
    n,e:=outbound.Read(buf)
    if e!=nil { return }
    peerMu.RLock(); recipient:=peer; peerMu.RUnlock()
    if recipient!=nil { if _,e=guest.WriteTo(buf[:n],recipient);e!=nil { return } }
   }
  }()
  buf:=make([]byte,65535)
  for {
   select {case <-g.ctx.Done(): return;default:}
   _ = guest.SetReadDeadline(time.Now().Add(60*time.Second))
   n,from,e := guest.ReadFrom(buf)
   if e!=nil { break }
   peerMu.Lock();peer=from;peerMu.Unlock()
   if _,e=outbound.Write(buf[:n]);e!=nil { break }
  }
  close(done)
  _=outbound.Close()
  wg.Wait()
 }()
}
