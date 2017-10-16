package netproxy

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	commandLocal        = 0
	commandProxy        = 1
	familyUnspecified   = 0
	familyInet          = 1
	familyInet6         = 2
	familyUnix          = 3
	protocolUnspecified = 0
	protocolStream      = 1
	protocolDgram       = 2
)

// Implements net.Listener, except this allows incoming connections to specify
// the real source of the connection via the PROXY command.
type ProxyListener struct {
	ProtocolDeadline time.Duration
	ProtocolError    func(error)
	acceptChan       chan net.Conn
	errorChan        chan error
	listener         net.Listener
	stop             int32
	waitGroup        sync.WaitGroup
}

func Listen(network, address string) (net.Listener, error) {
	listener, err := net.Listen(network, address)
	if err != nil {
		return nil, err
	}
	return ListenWrapper(listener), nil
}

func ListenWrapper(l net.Listener) *ProxyListener {
	p := &ProxyListener{
		listener:   l,
		acceptChan: make(chan net.Conn, 100),
		errorChan:  make(chan error),
	}
	p.waitGroup.Add(1)
	go p.goAcceptRoutine()
	return p
}

// Internal wrapper that will log the given error if the ProtocolError function
// is defined otherwise it does nothing.
func (p *ProxyListener) protocolErrorLog(
	conn net.Conn, msg string, args ...interface{},
) {
	if f := p.ProtocolError; f != nil {
		f(fmt.Errorf(msg, args...))
	}
	conn.Close()
}

// The singleton accept routine that accepts new connections and spins them
// off into goroutine handlers.
func (p *ProxyListener) goAcceptRoutine() {
	defer p.waitGroup.Done()
	for atomic.LoadInt32(&p.stop) == 0 {
		conn, err := p.listener.Accept()
		if err != nil {
			p.errorChan <- err
			continue
		}

		// Shard off the connection to a goroutine that will
		// read the PROXY header.
		p.waitGroup.Add(1)
		go p.goReadProxyRoutine(conn)
	}
	close(p.errorChan)
	close(p.acceptChan)
}

// The handler that takes a single connection and reads the PROXY line.
func (p *ProxyListener) goReadProxyRoutine(conn net.Conn) {
	defer p.waitGroup.Done()

	// We need to set the ProtocolDeadline on the connection.
	if p.ProtocolDeadline != 0 {
		deadline := time.Now().Add(p.ProtocolDeadline)
		if err := conn.SetDeadline(deadline); err != nil {
			p.protocolErrorLog(conn, "Error setting deadline on the socket: %s",
				err)
			return
		}
	}

	// Only read 1 byte so we can establish which protocol we are reading.
	fb := make([]byte, 1)
	if n, err := conn.Read(fb); err == io.EOF {
		p.protocolErrorLog(conn, "Connection closed before first byte.")
		return
	} else if err != nil {
		p.protocolErrorLog(conn, "Error reading first byte: %s", err)
		return
	} else if n != 1 {
		p.protocolErrorLog(conn, "Invalid number of bytes read: %d", n)
		return
	}

	switch fb[0] {
	case 'P':
		p.readVersion1(conn)
		return
	case 0x0D:
		p.readBinary(conn)
		return
	default:
		p.protocolErrorLog(conn, "Invalid first byte: %d", fb[0])
		return
	}
}

// This call will handle incoming Version 1 connections. Those are connections
// that operate in plain text, and start with 'PROXY' as the first bytes
// received off the connection.
func (p *ProxyListener) readVersion1(conn net.Conn) {
	// We need to do an unbuffered read since we don't want to over
	// read and start consuming the initial HTTP connection.
	//
	// 107 is the maximum defined in the document:
	// https://www.haproxy.org/download/1.8/doc/proxy-protocol.txt
	proxyLine := make([]byte, 107)
	proxyLineLen := 0

	// First we need to ensure that the text starts with the expected
	// PROXY line. If not then we can close the session as we would have
	// already consumed bytes which will screw up the underlying protocol
	// for any receiver. Note that we already read the 'P' in the protocol
	// detection component.
	expect := []byte("ROXY ")
	for proxyLineLen = 0; proxyLineLen < len(expect); {
		buffer := proxyLine[proxyLineLen:len(expect)]
		if n, err := conn.Read(buffer); err != nil {
			if err == io.EOF {
				p.protocolErrorLog(
					conn, "TCP session closed prior to PROXY line.")
			} else {
				p.protocolErrorLog(
					conn, "TCP error reading PROXY line: %s", err)
			}
			return
		} else {
			proxyLineLen += int(n)
		}
	}
	if !bytes.Equal(proxyLine[0:proxyLineLen], expect) {
		p.protocolErrorLog(
			conn, "First line did not start with 'P"+string(expect)+"'")
		return
	}

	// Loop through the rest of the first line from the socket.
	for i := proxyLineLen; ; i++ {
		if i >= len(proxyLine) {
			p.protocolErrorLog(conn, "PROXY line malformed (too long)")
			return
		} else if n, err := conn.Read(proxyLine[i : i+1]); err != nil {
			p.protocolErrorLog(conn, "Error reading PROXY line: %s", err)
			return
		} else if n != 1 {
			p.protocolErrorLog(
				conn, "Invalid number of bytes read from socket: %d", n)
			return
		} else if proxyLine[i] == '\r' {
			break
		} else {
			proxyLineLen += n
		}
	}

	// Validate the data from the line. We know that this will split into
	// at least 2 spots due to the "PROXY " check above.
	parts := bytes.Split(proxyLine[0:proxyLineLen], []byte(" "))
	if !bytes.Equal(parts[1], []byte("UNKNOWN")) {
		// Any other form must have 6 fields.
		//  - PROXY
		//  - TCP4 || TCP6
		//  - source ip
		//  - destination ip
		//  - source port
		//  - destination port
		if len(parts) != 6 {
			p.protocolErrorLog(
				conn,
				"PROXY line is malformed, incorrect number of elements: %d",
				len(parts))
			return
		}
		switch {
		case bytes.Equal(parts[1], []byte("TCP4")):
		case bytes.Equal(parts[1], []byte("TCP6")):
		default:
			p.protocolErrorLog(
				conn, "Protocol element of the PROXY line is invalid: %s",
				string(parts[1]))
			return
		}

		// Parse the source port.
		port, err := strconv.ParseInt(string(parts[4]), 10, 32)
		if err != nil {
			p.protocolErrorLog(
				conn,
				"destination port element of the PROXY line is not an int: %s",
				err)
			return
		}
		addr := &net.TCPAddr{
			IP:   net.ParseIP(string(parts[2])),
			Port: int(port),
		}
		conn = &connWrapper{Conn: conn, newRemoteAddr: addr}
	}

	// Now we disable the deadline so the connection will act as it would
	// in a default Listen() call.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		p.protocolErrorLog(
			conn, "Error unsetting deadline on the socket: %s", err)
		return
	}

	// Lastly, pass conn on to the Accept caller.
	p.acceptChan <- conn
	return
}

// This is called when a binary protocol header is read from the wire.
func (p *ProxyListener) readBinary(conn net.Conn) {
	// The binary protocol actually makes it easy to read the whole
	// network block in two passes rather than reading each individual part
	// one step at a time.
	//
	// Step one is to read the header, (of which we have already read a single
	// byte in the protocol selection stage.) This is formatted as such:
	//   12 - Fixed header bytes.
	//    1 - The version / command specifier.
	//    1 - The transport protocol / family specifier.
	//    2 - Address length in network order.
	// Once we read this block we will need to read a block as large as was
	// specified in the Address length field. We do this regardless of other
	// fields as this is required in the protocol specification.
	header := make([]byte, 11+1+1+2)
	for i := 0; i < len(header); {
		if n, err := conn.Read(header[i:]); err != nil {
			p.protocolErrorLog(
				conn, "Error reading binary header data: %s", err)
			return
		} else {
			i += n
		}
	}

	// Parse each field in the header.
	expect := []byte{
		0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x10,
	}
	if !bytes.Equal(header[0:len(expect)], expect) {
		p.protocolErrorLog(
			conn, "Static header bytes did not match expected value.")
		return
	}

	// Version.
	switch header[len(expect)] >> 4 {
	case 0x02:
	default:
		p.protocolErrorLog(conn, "Version in the header was not 2: %d",
			header[len(expect)]>>4)
		return
	}

	// Command bits.
	command := header[len(expect)] & 0x0F
	switch command {
	case commandLocal:
	case commandProxy:
	default:
		p.protocolErrorLog(conn, "Command bits contained an invalid value: %d",
			command)
		return
	}

	// Family bits.
	family := header[len(expect)+1] >> 4
	switch family {
	case familyUnspecified:
	case familyInet:
	case familyInet6:
	case familyUnix:
	default:
		p.protocolErrorLog(conn, "Family bits contained an invalid value: %d",
			family)
		return
	}

	// Protocol bits.
	protocol := header[len(expect)+1] & 0x0F
	switch protocol {
	case protocolUnspecified:
	case protocolStream:
	case protocolDgram:
	default:
		p.protocolErrorLog(conn,
			"Protocol bits contained an invalid value: %d", protocol)
		return
	}

	// Length is the last two bits.
	length := int(header[len(expect)+2])<<8 + int(header[len(expect)+3])

	// Sanity check on length to ensure that it is never too large.
	if length > 208 {
		p.protocolErrorLog(conn, "Length integer is too large to be valid: %d",
			length)
		return
	}

	// We need to read the bytes into a buffer..
	address := make([]byte, length)
	for i := 0; i < len(address); {
		if n, err := conn.Read(address[i:]); err != nil {
			p.protocolErrorLog(conn, "Error reading binary address data: %s",
				err)
			return
		} else {
			i += n
		}
	}

	// Regardless of outcome at this point we need to unset the Deadline timer
	// so we go ahead and do that here.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		p.protocolErrorLog(conn, "Error unsetting deadline on the socket: %s",
			err)
		return
	}

	// If the command family is listed as LOCAL then we don't do any processing
	// on it at all. We just pass the connection directly on to the Accept()
	// function. This is typically used for health checks. We also pass through
	// any connection with an unspecified family or protocol.
	switch {
	case command != commandLocal:
	case family != familyUnspecified:
	case protocol != protocolUnspecified:
	default:
		p.acceptChan <- conn
		return
	}

	// TCP connections over IPv4
	switch {
	case family != familyInet:
	case protocol != protocolStream:
	default:
		if len(address) != 12 {
			p.protocolErrorLog(conn,
				"Invalid address length for a INET proxy (should be 12): %d",
				len(address))
			return
		}
		addr := &net.TCPAddr{
			IP:   net.IPv4(address[0], address[1], address[2], address[3]),
			Port: int(address[8])<<8 + int(address[9]),
		}
		conn = &connWrapper{Conn: conn, newRemoteAddr: addr}
		p.acceptChan <- conn
		return
	}

	// UDP connections over IPv4
	switch {
	case family != familyInet:
	case protocol != protocolDgram:
	default:
		if len(address) != 12 {
			p.protocolErrorLog(conn,
				"Invalid address length for a INET proxy (should be 12): %d",
				len(address))
			return
		}
		addr := &net.UDPAddr{
			IP:   net.IPv4(address[0], address[1], address[2], address[3]),
			Port: int(address[8])<<8 + int(address[9]),
		}
		conn = &connWrapper{Conn: conn, newRemoteAddr: addr}
		p.acceptChan <- conn
		return
	}

	// TCP connections over IPv6
	switch {
	case family != familyInet6:
	case protocol != protocolStream && family != protocolDgram:
	default:
		if len(address) != 36 {
			p.protocolErrorLog(conn,
				"Invalid address length for a INET6 proxy (should be 36): %d",
				len(address))
			return
		}
		// We need to copy the array rather than using a slice because we don't
		// want the whole address block persisted for the length of the
		// connection.
		newAddr := make([]byte, 16)
		copy(newAddr, address[0:16])
		addr := &net.TCPAddr{
			IP:   net.IP(newAddr),
			Port: int(address[32])<<8 + int(address[33]),
		}
		conn = &connWrapper{Conn: conn, newRemoteAddr: addr}
		p.acceptChan <- conn
		return
	}

	// FIXME: Add Dgram support?
	p.protocolErrorLog(conn, "Unsupported family/protocol combination.")
	return
}

// Returns the address of the listening socket.
func (p *ProxyListener) Addr() net.Addr {
	return p.listener.Addr()
}

// This will pop the next connection off the queue. This connection should
// ideally have its Addr() set to match the address passed into us using
// the PROXY command. If the data was unknown then the address will not
// be touched and will hold the incoming address of the load balancer.
//
// This actually pops already connected sockets off a channel so that it will
// act just like the Accept() call in net.Listener. Closing the socket will
// cause this to return an error.
func (p *ProxyListener) Accept() (net.Conn, error) {
	// Accept actually reads connections off the channel that were already
	// accepted and processed by the runner goroutine.
	select {
	case err := <-p.errorChan:
		return nil, err
	case conn := <-p.acceptChan:
		return conn, nil
	}
}

// Like net.Listener this will close the underlying socket. Unlike net.Listener
// this is not sufficient to ensure that the underlying objects all get purged.
// Since this object has a running goroutine you must call Stop() to be
// absolutely sure that everything is closed properly.
func (p *ProxyListener) Close() error {
	// We need to stop the accepting thread by closing the socket. This in
	// turn will write an error to the error channel which will stop any
	// callers in Accept().
	err := p.listener.Close()
	atomic.StoreInt32(&p.stop, 1)
	return err
}

// Ensures that all goroutines associated with this listener are shutdown and
// closed out.
func (p *ProxyListener) Stop() {
	atomic.StoreInt32(&p.stop, 1)
	p.listener.Close()
	for range p.errorChan {
	}
	for range p.acceptChan {
	}
	p.waitGroup.Wait()
}

// This is a wrapper around net.TCPConn that lets us overwrite the RemoteAddr
// function so we can return the remoteAddr provided by the PROXY line.
type connWrapper struct {
	net.Conn
	newRemoteAddr net.Addr
}

func (t *connWrapper) RemoteAddr() net.Addr {
	return t.newRemoteAddr
}
