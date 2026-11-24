//go:build tinygo && cyw43439

// CYW43439 WiFi backend (Interstate 75 W and other Pico-W-class boards).
//
// Two halves, in this order below:
//
//   - THE STACK. The cyw43439 chip plus the lneto TCP/IP glue — inlined and trimmed from
//     soypat's examples/cywnet so we depend on the driver and lneto directly rather than on
//     example code. Connect joins the network, runs DHCP, and pumps packets in the
//     background.
//   - THE NETDEV BRIDGE. A netdev.Netdever over that stack, so net and net/http work here.
//     TinyGo's net package contains no networking: it delegates every socket call to a
//     Netdever installed with netdev.UseNetdev. Boards with a vendor stack (ninafw,
//     espradio) ship one; lneto exposes tcp.Conn rather than a socket table, so this file
//     bridges the gap. With the bridge registered, every target fetches the same way.
package wifi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"machine"
	"net/netip"
	"time"

	"github.com/soypat/cyw43439"
	"github.com/soypat/lneto/dns"
	"github.com/soypat/lneto/tcp"
	"github.com/soypat/lneto/tcp/rto"
	"github.com/soypat/lneto/x/xnet"
	"tinygo.org/x/drivers/netdev"
)

const (
	// hostname is the DHCP option-12 name the router records against the lease —
	// deliberately non-identifying. lneto refuses an empty one (Reset returns
	// ErrInvalidConfig), so the option can't simply be omitted.
	hostname   = "anonymous"
	tcpBufSize = 2030 // MTU minus eth/ip/tcp headers

	// Timeouts for the netdev's socket calls. net gives Dial no deadline of its own,
	// so these bound it; Send/Recv deadlines come from the caller.
	dialTimeout = 5 * time.Second
	dialRetries = 3
	dnsTimeout  = 3 * time.Second
	dnsRetries  = 2

	// ioIdleTimeout bounds a Send or Recv the caller gave no deadline, which is every one
	// net/http makes here: TinyGo's client can't carry a timeout (see internal/client's
	// http_tinygo.go). Without it a stalled connection blocks the fetch until lneto gives up
	// on its own, which on hardware took about five minutes — long enough for the board to
	// grey everything out as outdated. The server answers from memory in milliseconds, so
	// this much silence means the connection is gone; failing lets the next poll redial.
	ioIdleTimeout = 10 * time.Second
)

// stack bridges the cyw43439 device to an lneto TCP/IP stack.
type stack struct {
	s       xnet.StackAsync
	dev     *cyw43439.Device
	sendbuf []byte
}

// active is the connected stack, set by Connect and read by the netdev bridge.
var active *stack

// Connect initializes the CYW43439, joins ssid, runs DHCP, and starts the background pump
// that moves packets between the device and the stack. Requires -scheduler=tasks.
func Connect(ssid, pass string) error {
	dev := cyw43439.NewPicoWDevice()
	dev.SetLogger(slog.New(slog.NewTextHandler(machine.Serial, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err := dev.Init(cyw43439.DefaultWifiConfig()); err != nil {
		return err
	}
	if err := dev.Join(ssid, cyw43439.JoinOptions{Passphrase: pass}); err != nil {
		return err
	}
	mac, err := dev.HardwareAddr6()
	if err != nil {
		return err
	}

	st := &stack{dev: dev, sendbuf: make([]byte, cyw43439.MaxFrameSize)}
	if err := st.s.Reset(xnet.StackConfig{
		Hostname:          hostname,
		MaxActiveTCPPorts: 1,
		HardwareAddress:   mac,
		MTU:               1500,
		RandSeed:          time.Now().UnixNano(),
	}); err != nil {
		return err
	}
	dev.RecvEthHandle(func(pkt []byte) { st.s.IngressEthernet(pkt) }) // malformed/unwanted frames are just dropped

	// The stack only moves packets when driven; pump it forever in the background while the
	// display refreshes from its own DMA/timer.
	go func() {
		for {
			send, recv := st.recvAndSend()
			if send == 0 && recv == 0 {
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	res, err := st.setupDHCP()
	if err != nil {
		return err
	}
	println("wifi: joined, DHCP address", netip.AddrFrom4(res.AssignedAddr4).String())
	// The stack is up and addressed: expose it to net/net/http.
	if err := registerNetdev(); err != nil {
		return err
	}

	active = st
	return nil
}

// resolve turns a hostname into an IP address over DNS, with retries. The DNS
// server comes from the DHCP lease: setupDHCP hands the whole result to
// AssimilateDHCPResults, which keeps DNSServers[0].
//
// A name that already parses as an IP address is returned unchanged rather than
// asked about, so a config pointing at a literal address never touches the network.
// net calls this through the bridge's GetHostByName.
func resolve(host string, timeout time.Duration, retries int) (netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr, nil
	}
	if active == nil {
		return netip.Addr{}, errors.New("wifi: not connected")
	}
	var name dns.Name
	if err := name.Parse(host); err != nil {
		return netip.Addr{}, err
	}
	var addrs [4]netip.Addr
	n, err := active.s.StackRetrying(defaultBackoff).DoLookupIP(addrs[:], name, timeout, retries)
	if err != nil {
		return netip.Addr{}, err
	}
	for _, a := range addrs[:n] {
		if a.Is4() {
			return a, nil // the stack is IPv4-only; skip any AAAA that slips through
		}
	}
	return netip.Addr{}, errors.New("wifi: no IPv4 address for " + host)
}

// recvAndSend moves one inbound and one outbound frame between the device and the stack.
// The byte counts let the caller back off when both are idle (0/0).
func (st *stack) recvAndSend() (send, recv int) {
	if gotRecv, _ := st.dev.PollOne(); gotRecv {
		recv = 1
	}
	send, err := st.s.EgressEthernet(st.sendbuf)
	if err == nil && send > 0 {
		st.dev.SendEth(st.sendbuf[:send])
	}
	return send, recv
}

// setupDHCP runs a DHCP exchange, adopts the lease, and resolves the gateway's MAC.
func (st *stack) setupDHCP() (*xnet.DHCPResults, error) {
	rstack := st.s.StackRetrying(defaultBackoff)
	res, err := rstack.DoDHCPv4([4]byte{}, 3*time.Second, 3)
	if err != nil {
		return nil, err
	}
	if err := st.s.AssimilateDHCPResults(res); err != nil {
		return nil, err
	}
	gwHW, err := rstack.DoResolveHardwareAddress6(res.Router, 500*time.Millisecond, 4)
	if err != nil {
		return nil, err
	}
	st.s.SetGatewayHardwareAddr(gwHW)
	return res, nil
}

// defaultBackoff is the DHCP/retry backoff (inlined from cywnet.DefaultStackBackoff): 100 µs
// doubling to a 20 ms ceiling. For request/reply protocols, not TCP streams.
func defaultBackoff(consecutive uint) time.Duration {
	const (
		minWait  = 100 * uint32(time.Microsecond)
		maxWait  = 20 * uint32(time.Millisecond)
		maxShift = 15
	)
	sleep := minWait << min(consecutive, maxShift)
	if sleep > maxWait {
		sleep = maxWait
	}
	return time.Duration(sleep)
}

// ─── the netdev bridge ───────────────────────────────────────────────────────────

// maxSockets bounds the socket table. The clock holds one connection at a time — a poll
// dials, fetches and closes — so two is already slack for an overlapping close.
//
// Dropping to 1 would save ~4 KB of buffers but is deliberately deferred: the memory
// failures seen on device were fragmentation from churn, which headroom doesn't fix (see
// the repo README, "Memory on TinyGo"), and with one slot a leaked fd turns fatal — every
// later fetch returns errNoSockets until reboot — where two slots leave a working socket.
const maxSockets = 2

// connBackoff is how a blocked Read/Write/Flush waits for the stack pump: 1 µs doubling to a
// 5 ms ceiling (from cyw43439's examples/http-bench). Short, because the pump moves a frame
// every few ms and a TCP stream stalls for as long as we sleep.
func connBackoff(consecutive uint) time.Duration {
	const (
		minWait  = uint32(time.Microsecond)
		maxWait  = 5 * uint32(time.Millisecond)
		maxShift = 22
	)
	wait := minWait << min(consecutive, maxShift)
	if wait > maxWait {
		wait = maxWait
	}
	return time.Duration(wait)
}

var (
	errUnsupported = errors.New("wifi: operation not supported by the lneto netdev")
	errBadFD       = errors.New("wifi: bad socket descriptor")
	errNoSockets   = errors.New("wifi: socket table full")
	errNotConn     = errors.New("wifi: not connected")
	errDropped     = errors.New("wifi: connection dropped")
)

// socket is one entry in the table. The buffers are allocated once with the table and
// reused across connections: they are the largest thing a fetch touches, and cycling them
// per poll is what a non-compacting heap likes least.
//
// rtx is the connection's loss recovery: an RFC 6298 retransmission timer, handed to
// lneto as the conn's Policy. Without a Policy lneto never retransmits what we send, so a
// lost request segment would stall the fetch until ioIdleTimeout. lneto resets the timer
// whenever the conn opens or aborts, so one per slot serves every connection.
type socket struct {
	conn  tcp.Conn
	rtx   rto.Timer
	rx    []byte
	tx    []byte
	inUse bool
}

// lnetoNetdev implements netdev.Netdever over the connected lneto stack.
type lnetoNetdev struct {
	socks   [maxSockets]socket
	nextEph uint16 // offset into the ephemeral range of the next local port; see ephemeralPort
}

// bootTime anchors the monotonic clock the retransmission timers run on.
var bootTime = time.Now()

func nanotime() int64 { return int64(time.Since(bootTime)) }

// ephemeralPort picks the local port for the next dial: the next one in the IANA dynamic
// range (49152-65535), starting from a random point. Sequential, as lneto's own dialer does
// it, because picking at random reuses a recent port surprisingly often, and a reused
// address pair can run into state the previous connection left behind (a TIME-WAIT on the
// server, a flow entry in the router) that swallows the new SYN.
func (d *lnetoNetdev) ephemeralPort() uint16 {
	if d.nextEph == 0 {
		d.nextEph = uint16(active.s.Prand32()%16384) | 1
	}
	port := 49152 + d.nextEph%16384
	d.nextEph++
	return port
}

var theNetdev lnetoNetdev

// registerNetdev installs the bridge and preallocates the socket buffers. Called from
// Connect once the stack is up.
func registerNetdev() error {
	for i := range theNetdev.socks {
		s := &theNetdev.socks[i]
		s.rx = make([]byte, tcpBufSize)
		s.tx = make([]byte, tcpBufSize)
		if err := s.rtx.Configure(nanotime); err != nil {
			return err
		}
	}
	netdev.UseNetdev(&theNetdev)
	return nil
}

func (d *lnetoNetdev) sock(fd int) (*socket, error) {
	if fd < 0 || fd >= len(d.socks) || !d.socks[fd].inUse {
		return nil, errBadFD
	}
	return &d.socks[fd], nil
}

// GetHostByName resolves a name to an IPv4 address. net calls this for every Dial that
// isn't already a literal address.
func (d *lnetoNetdev) GetHostByName(name string) (netip.Addr, error) {
	return resolve(name, dnsTimeout, dnsRetries)
}

// Addr reports the address DHCP assigned us.
func (d *lnetoNetdev) Addr() (netip.Addr, error) {
	if active == nil {
		return netip.Addr{}, errNotConn
	}
	return netip.AddrFrom4(active.s.Addr4()), nil
}

// Socket claims a table slot. Only IPv4 stream sockets are served; see the package note
// on why UDP is refused.
func (d *lnetoNetdev) Socket(domain, stype, protocol int) (int, error) {
	if domain != netdev.AF_INET || stype != netdev.SOCK_STREAM {
		return -1, errUnsupported
	}
	if active == nil {
		return -1, errNotConn
	}
	for i := range d.socks {
		if !d.socks[i].inUse {
			d.socks[i].inUse = true
			return i, nil
		}
	}
	return -1, errNoSockets
}

// Connect opens the TCP connection. net always passes the resolved AddrPort and an empty
// host, having already called GetHostByName if it needed to.
func (d *lnetoNetdev) Connect(fd int, host string, ip netip.AddrPort) error {
	s, err := d.sock(fd)
	if err != nil {
		return err
	}
	if err := s.conn.Configure(tcp.ConnConfig{
		RxBuf:             s.rx,
		TxBuf:             s.tx,
		TxPacketQueueSize: 3,
		RWBackoff:         connBackoff, // required since lneto 0.4 (else ErrMissingHALConfig)
		Policy:            &s.rtx,
	}); err != nil {
		return err
	}
	rstack := active.s.StackRetrying(defaultBackoff)
	return rstack.DoDialTCP(context.Background(), &s.conn, d.ephemeralPort(), ip, dialTimeout, dialRetries)
}

// Send writes to the connection. lneto's Write blocks (backing off with connBackoff) until
// the data is queued or the conn's write deadline passes. The zero deadline net passes when
// the caller set none becomes ioIdleTimeout from now.
func (d *lnetoNetdev) Send(fd int, buf []byte, flags int, deadline time.Time) (int, error) {
	s, err := d.sock(fd)
	if err != nil {
		return -1, err
	}
	if deadline.IsZero() {
		deadline = time.Now().Add(ioIdleTimeout)
	}
	if err := s.conn.SetWriteDeadline(deadline); err != nil {
		return -1, err
	}
	n, err := s.conn.Write(buf)
	if err != nil {
		return n, err
	}
	// lneto queues; the stack pump is what actually puts it on the wire.
	if err := s.conn.Flush(); err != nil {
		return n, err
	}
	return n, nil
}

// Recv reads from the connection. lneto's Read blocks until data arrives, the peer closes
// (io.EOF), the connection is dropped (net.ErrClosed), or the conn's read deadline passes.
// As with Send, no deadline becomes ioIdleTimeout from now.
//
// It must return io.EOF EXACTLY on a closed peer, and only then: net.TCPConn.Read
// special-cases io.EOF and wraps every other error in an OpError. An EOF reported as
// anything else surfaces as a read failure rather than a clean end of body, which is how a
// Content-Length-less response would break; and a dropped connection reported as EOF reads
// as the server hanging up (or, mid-body, as "unexpected EOF"), hiding what happened.
func (d *lnetoNetdev) Recv(fd int, buf []byte, flags int, deadline time.Time) (int, error) {
	s, err := d.sock(fd)
	if err != nil {
		return -1, err
	}
	if deadline.IsZero() {
		deadline = time.Now().Add(ioIdleTimeout)
	}
	if err := s.conn.SetReadDeadline(deadline); err != nil {
		if s.conn.State().IsClosed() {
			return 0, errDropped
		}
		return -1, err
	}
	n, err := s.conn.Read(buf)
	if n > 0 {
		return n, nil // hand the data over; report any error on the next call
	}
	if err == io.EOF {
		return 0, io.EOF
	}
	if err != nil && s.conn.State().IsClosed() {
		return 0, errDropped
	}
	return 0, err
}

// Close ends the connection and frees the slot. The buffers stay with the table.
func (d *lnetoNetdev) Close(fd int) error {
	s, err := d.sock(fd)
	if err != nil {
		return err
	}
	s.conn.Close()
	for i := 0; i < 20 && !s.conn.State().IsClosed(); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	s.conn.Abort()
	s.inUse = false
	return nil
}

// SetSockOpt accepts and ignores the options net sets on a client socket (SO_KEEPALIVE,
// SO_LINGER, keepalive intervals). lneto exposes no equivalent knobs, and refusing them
// would fail Dial for options that only tune a connection this firmware holds for under a
// second. Anything else is refused rather than silently dropped.
func (d *lnetoNetdev) SetSockOpt(fd int, level, opt int, value interface{}) error {
	if _, err := d.sock(fd); err != nil {
		return err
	}
	switch level {
	case netdev.SOL_SOCKET, netdev.SOL_TCP:
		return nil
	}
	return errUnsupported
}

// Server-side operations. Nothing listens on the panel; see the package note.
func (d *lnetoNetdev) Bind(fd int, ip netip.AddrPort) error { return errUnsupported }
func (d *lnetoNetdev) Listen(fd int, backlog int) error     { return errUnsupported }
func (d *lnetoNetdev) Accept(fd int) (int, netip.AddrPort, error) {
	return -1, netip.AddrPort{}, errUnsupported
}

// Compile-time assertion that the bridge satisfies the interface net expects.
var _ netdev.Netdever = (*lnetoNetdev)(nil)
