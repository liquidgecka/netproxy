package netproxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	v2Version           = 0x02
	commandLocal        = 0
	commandProxy        = 1
	familyUnspecified   = 0
	familyInet          = 1
	familyInet6         = 2
	familyUnix          = 3
	protocolUnspecified = 0
	protocolStream      = 1
	protocolDgram       = 2

	maxV1LineLen      = 107
	v1Prefix          = "PROXY "
	v2HeaderRemainLen = 15
	v2IPv4AddrLen     = 12
	v2IPv6AddrLen     = 36
	v2UnixAddrLen     = 216
	v2UnixPathLen     = 108
	acceptQueueSize   = 100
)

var v2Signature = [12]byte{
	0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A,
}

var _ net.Listener = (*ProxyListener)(nil)

// Option configures a [*ProxyListener] before it starts accepting connections.
type Option func(*ProxyListener)

// WithProtocolDeadline sets the maximum time allowed to read a complete PROXY
// header from a newly accepted connection. A zero duration disables the deadline.
func WithProtocolDeadline(d time.Duration) Option {
	return func(p *ProxyListener) {
		p.ProtocolDeadline = d
	}
}

// WithProtocolError sets the callback invoked when an incoming connection fails
// PROXY protocol parsing or deadline setup. The callback may be invoked
// concurrently from multiple goroutines.
func WithProtocolError(fn func(error)) Option {
	return func(p *ProxyListener) {
		p.ProtocolError = fn
	}
}

// ProxyListener implements [net.Listener], parsing incoming HAProxy PROXY
// protocol v1 or v2 headers so accepted connections report the client's
// proxied remote address via RemoteAddr.
type ProxyListener struct {
	// ProtocolDeadline limits how long a newly accepted connection has to
	// send a complete PROXY header. Zero disables the deadline. Configure via
	// [WithProtocolDeadline]; must not be modified after starting.
	ProtocolDeadline time.Duration

	// ProtocolError is called when an incoming connection fails PROXY
	// protocol parsing or deadline setup before closing the connection.
	// It may be called concurrently from multiple goroutines. Configure via
	// [WithProtocolError]; must not be modified after starting.
	ProtocolError func(error)

	acceptChan chan net.Conn
	errorChan  chan error
	done       chan struct{}
	listener   net.Listener
	closed     atomic.Bool
	mu         sync.Mutex
	pending    map[net.Conn]struct{}
	acceptWG   sync.WaitGroup
	connWG     sync.WaitGroup
	stopOnce   sync.Once
}

// Listen announces on the local network address and wraps the resulting
// [net.Listener] in a [*ProxyListener].
func Listen(network, address string, opts ...Option) (net.Listener, error) {
	listener, err := net.Listen(network, address)
	if err != nil {
		return nil, err
	}
	return ListenWrapper(listener, opts...), nil
}

// ListenWrapper wraps an existing [net.Listener] so accepted connections
// must present a valid PROXY protocol v1 or v2 header.
func ListenWrapper(l net.Listener, opts ...Option) *ProxyListener {
	p := &ProxyListener{
		listener:   l,
		acceptChan: make(chan net.Conn, acceptQueueSize),
		errorChan:  make(chan error),
		done:       make(chan struct{}),
		pending:    make(map[net.Conn]struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(p)
		}
	}
	p.acceptWG.Go(p.acceptLoop)
	return p
}

func (p *ProxyListener) acceptLoop() {
	for !p.closed.Load() {
		conn, err := p.listener.Accept()
		if err != nil {
			if p.closed.Load() {
				return
			}
			select {
			case p.errorChan <- err:
			case <-p.done:
				return
			}
			continue
		}

		p.connWG.Go(func() {
			p.handleConn(conn)
		})
	}
}

func (p *ProxyListener) handleConn(conn net.Conn) {
	if !p.trackPending(conn) {
		_ = conn.Close()
		return
	}

	wrapped, err := p.readProxyHeader(conn)
	removed := p.untrackPending(conn)
	if err != nil {
		if removed {
			p.reportProtocolError(conn, err)
		} else {
			_ = conn.Close()
		}
		return
	}

	if !removed || p.closed.Load() {
		_ = wrapped.Close()
		return
	}

	select {
	case p.acceptChan <- wrapped:
	case <-p.done:
		_ = wrapped.Close()
	}
}

func (p *ProxyListener) trackPending(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() {
		return false
	}
	if p.pending == nil {
		p.pending = make(map[net.Conn]struct{})
	}
	p.pending[conn] = struct{}{}
	return true
}

func (p *ProxyListener) untrackPending(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.pending[conn]; !ok {
		return false
	}
	delete(p.pending, conn)
	return true
}

func (p *ProxyListener) reportProtocolError(conn net.Conn, err error) {
	if !p.closed.Load() && p.ProtocolError != nil {
		p.ProtocolError(err)
	}
	_ = conn.Close()
}

func (p *ProxyListener) readProxyHeader(conn net.Conn) (net.Conn, error) {
	if p.ProtocolDeadline != 0 {
		if err := conn.SetDeadline(time.Now().Add(p.ProtocolDeadline)); err != nil {
			return nil, fmt.Errorf("setting protocol deadline: %w", err)
		}
	}

	var first [1]byte
	if _, err := io.ReadFull(conn, first[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("connection closed before first byte")
		}
		return nil, fmt.Errorf("reading first byte: %w", err)
	}

	var (
		wrapped net.Conn
		err     error
	)
	switch first[0] {
	case 'P':
		wrapped, err = readVersion1(conn)
	case v2Signature[0]:
		wrapped, err = readBinary(conn)
	default:
		return nil, fmt.Errorf("invalid first byte: %d", first[0])
	}
	if err != nil {
		return nil, err
	}

	if p.ProtocolDeadline != 0 {
		if err := conn.SetDeadline(time.Time{}); err != nil {
			return nil, fmt.Errorf("clearing protocol deadline: %w", err)
		}
	}
	return wrapped, nil
}

// readVersion1 parses a plain-text PROXY protocol v1 line starting after the
// leading 'P' byte that was already read during protocol detection.
func readVersion1(conn net.Conn) (net.Conn, error) {
	// 107 bytes is the maximum v1 line length including CRLF defined by the
	// HAProxy specification.
	var proxyLine [maxV1LineLen]byte
	proxyLine[0] = 'P'

	if _, err := io.ReadFull(conn, proxyLine[1:len(v1Prefix)]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errors.New("connection closed prior to PROXY prefix")
		}
		return nil, fmt.Errorf("reading PROXY prefix: %w", err)
	}
	if !bytes.Equal(proxyLine[:len(v1Prefix)], []byte(v1Prefix)) {
		return nil, errors.New("header did not start with 'PROXY '")
	}

	// Read byte-by-byte to avoid consuming application payload past CRLF.
	lineEnd := -1
	for i := len(v1Prefix); i < len(proxyLine); i++ {
		if _, err := io.ReadFull(conn, proxyLine[i:i+1]); err != nil {
			return nil, fmt.Errorf("reading PROXY line: %w", err)
		}
		if proxyLine[i] == '\n' {
			return nil, errors.New("malformed PROXY line: bare LF without CR")
		}
		if proxyLine[i] == '\r' {
			if i+1 >= len(proxyLine) {
				return nil, errors.New("malformed PROXY line: exceeds maximum length")
			}
			if _, err := io.ReadFull(conn, proxyLine[i+1:i+2]); err != nil {
				return nil, fmt.Errorf("reading PROXY line CRLF: %w", err)
			}
			if proxyLine[i+1] != '\n' {
				return nil, errors.New("malformed PROXY line: CR not followed by LF")
			}
			lineEnd = i
			break
		}
	}
	if lineEnd < 0 {
		return nil, errors.New("malformed PROXY line: exceeds maximum length")
	}

	addr, err := parseVersion1Line(proxyLine[:lineEnd])
	if err != nil {
		return nil, err
	}
	if addr == nil {
		return conn, nil
	}
	return &connWrapper{Conn: conn, newRemoteAddr: addr}, nil
}

func parseVersion1Line(line []byte) (net.Addr, error) {
	parts := bytes.Split(line, []byte(" "))
	if len(parts) < 2 {
		return nil, fmt.Errorf("malformed PROXY line: incorrect number of elements: %d", len(parts))
	}
	if bytes.Equal(parts[1], []byte("UNKNOWN")) {
		return nil, nil
	}
	if len(parts) != 6 {
		return nil, fmt.Errorf("malformed PROXY line: incorrect number of elements: %d", len(parts))
	}

	proto := string(parts[1])
	if proto != "TCP4" && proto != "TCP6" {
		return nil, fmt.Errorf("invalid PROXY protocol %q", proto)
	}

	srcIP, err := netip.ParseAddr(string(parts[2]))
	if err != nil {
		return nil, fmt.Errorf("invalid source IP %q: %w", string(parts[2]), err)
	}
	dstIP, err := netip.ParseAddr(string(parts[3]))
	if err != nil {
		return nil, fmt.Errorf("invalid destination IP %q: %w", string(parts[3]), err)
	}

	switch proto {
	case "TCP4":
		if !srcIP.Is4() || !dstIP.Is4() {
			return nil, errors.New("protocol TCP4 requires IPv4 addresses")
		}
	case "TCP6":
		if !srcIP.Is6() || srcIP.Is4In6() || srcIP.Zone() != "" ||
			!dstIP.Is6() || dstIP.Is4In6() || dstIP.Zone() != "" {
			return nil, errors.New("protocol TCP6 requires IPv6 addresses")
		}
	}

	srcPort, err := strconv.ParseUint(string(parts[4]), 10, 16)
	if err != nil {
		return nil, fmt.Errorf("invalid source port %q: %w", string(parts[4]), err)
	}
	if _, err := strconv.ParseUint(string(parts[5]), 10, 16); err != nil {
		return nil, fmt.Errorf("invalid destination port %q: %w", string(parts[5]), err)
	}

	return net.TCPAddrFromAddrPort(netip.AddrPortFrom(srcIP, uint16(srcPort))), nil
}

// readBinary parses a PROXY protocol v2 binary header starting after the
// leading 0x0D signature byte read during protocol detection.
func readBinary(conn net.Conn) (net.Conn, error) {
	// Remaining fixed header after the first signature byte (0x0D):
	//  11 - remaining signature bytes
	//   1 - version (high 4 bits) and command (low 4 bits)
	//   1 - address family (high 4 bits) and transport protocol (low 4 bits)
	//   2 - big-endian payload length
	var header [v2HeaderRemainLen]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, fmt.Errorf("reading binary header: %w", err)
	}

	if !bytes.Equal(header[:11], v2Signature[1:]) {
		return nil, errors.New("binary signature mismatch")
	}

	version := header[11] >> 4
	if version != v2Version {
		return nil, fmt.Errorf("unsupported binary protocol version: %d", version)
	}

	command := header[11] & 0x0F
	if command != commandLocal && command != commandProxy {
		return nil, fmt.Errorf("invalid binary command: %d", command)
	}

	family := header[12] >> 4
	if family > familyUnix {
		return nil, fmt.Errorf("invalid binary address family: %d", family)
	}

	protocol := header[12] & 0x0F
	if protocol > protocolDgram {
		return nil, fmt.Errorf("invalid binary transport protocol: %d", protocol)
	}

	length := int(binary.BigEndian.Uint16(header[13:15]))
	var address []byte
	if length > 0 {
		address = make([]byte, length)
		if _, err := io.ReadFull(conn, address); err != nil {
			return nil, fmt.Errorf("reading binary address payload: %w", err)
		}
	}

	if command == commandLocal || family == familyUnspecified || protocol == protocolUnspecified {
		return conn, nil
	}

	addr, err := parseBinaryAddr(family, protocol, address)
	if err != nil {
		return nil, err
	}
	return &connWrapper{Conn: conn, newRemoteAddr: addr}, nil
}

func parseBinaryAddr(family, protocol byte, address []byte) (net.Addr, error) {
	switch family {
	case familyInet:
		if len(address) < v2IPv4AddrLen {
			return nil, fmt.Errorf("invalid IPv4 proxy address length: got %d, want at least %d", len(address), v2IPv4AddrLen)
		}
		ip := net.IPv4(address[0], address[1], address[2], address[3])
		port := int(binary.BigEndian.Uint16(address[8:10]))
		switch protocol {
		case protocolStream:
			return &net.TCPAddr{IP: ip, Port: port}, nil
		case protocolDgram:
			return &net.UDPAddr{IP: ip, Port: port}, nil
		}

	case familyInet6:
		if len(address) < v2IPv6AddrLen {
			return nil, fmt.Errorf("invalid IPv6 proxy address length: got %d, want at least %d", len(address), v2IPv6AddrLen)
		}
		// Clone the 16-byte slice so the rest of the payload buffer can be GC'd.
		ip := net.IP(slices.Clone(address[0:16]))
		port := int(binary.BigEndian.Uint16(address[32:34]))
		switch protocol {
		case protocolStream:
			return &net.TCPAddr{IP: ip, Port: port}, nil
		case protocolDgram:
			return &net.UDPAddr{IP: ip, Port: port}, nil
		}

	case familyUnix:
		if len(address) < v2UnixAddrLen {
			return nil, fmt.Errorf("invalid UNIX proxy address length: got %d, want at least %d", len(address), v2UnixAddrLen)
		}
		srcPath := address[:v2UnixPathLen]
		if end := bytes.IndexByte(srcPath, 0); end >= 0 {
			srcPath = srcPath[:end]
		}
		name := string(srcPath)
		switch protocol {
		case protocolStream:
			return &net.UnixAddr{Name: name, Net: "unix"}, nil
		case protocolDgram:
			return &net.UnixAddr{Name: name, Net: "unixgram"}, nil
		}
	}

	return nil, fmt.Errorf("unsupported family/protocol combination: family=%d protocol=%d", family, protocol)
}

// Addr returns the listener's network address.
func (p *ProxyListener) Addr() net.Addr {
	return p.listener.Addr()
}

// Accept waits for and returns the next connection to the listener after
// parsing its PROXY header. The returned connection's RemoteAddr reflects
// the proxied client address when provided.
func (p *ProxyListener) Accept() (net.Conn, error) {
	if p.closed.Load() {
		return nil, net.ErrClosed
	}
	select {
	case err := <-p.errorChan:
		return nil, err
	case conn := <-p.acceptChan:
		return conn, nil
	case <-p.done:
		return nil, net.ErrClosed
	}
}

// Close stops accepting new connections, closes any connections currently
// reading a PROXY header, and closes the underlying [net.Listener].
// Unlike [ProxyListener.Stop], Close does not wait for background goroutines
// to exit.
func (p *ProxyListener) Close() error {
	var toClose []net.Conn
	p.mu.Lock()
	if !p.closed.Swap(true) {
		if p.done != nil {
			close(p.done)
		}
		if len(p.pending) > 0 {
			toClose = make([]net.Conn, 0, len(p.pending))
			for c := range p.pending {
				toClose = append(toClose, c)
			}
			clear(p.pending)
		}
	}
	p.mu.Unlock()

	for _, c := range toClose {
		_ = c.Close()
	}
	if p.listener != nil {
		return p.listener.Close()
	}
	return nil
}

// Stop closes the listener, waits for all background accept and header
// parsing goroutines to finish, and closes any parsed connections still
// waiting in the accept queue.
func (p *ProxyListener) Stop() {
	p.stopOnce.Do(func() {
		_ = p.Close()
		p.acceptWG.Wait()
		p.connWG.Wait()
		for {
			select {
			case conn := <-p.acceptChan:
				_ = conn.Close()
			default:
				return
			}
		}
	})
}

// connWrapper wraps a [net.Conn] to override RemoteAddr with the address
// parsed from a PROXY protocol header.
type connWrapper struct {
	net.Conn
	newRemoteAddr net.Addr
}

func (c *connWrapper) RemoteAddr() net.Addr {
	return c.newRemoteAddr
}

// NetConn returns the underlying [net.Conn].
func (c *connWrapper) NetConn() net.Conn {
	return c.Conn
}
