package netproxy

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// Implements net.Listener, except this allows incoming connections to specify
// the real source of the connection via the PROXY command.
type ProxyListener struct {
	ProtocolDeadline time.Duration
	ProtocolError    func(error)
	acceptChan       chan net.Conn
	errorChan        chan error
	listener         net.Listener
	stop             bool
	waitGroup        sync.WaitGroup
}

func Listen(network, address string) (*ProxyListener, error) {
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
func (p *ProxyListener) protocolErrorLog(err error) {
	if f := p.ProtocolError; f != nil {
		f(err)
	}
}

// The singleton accept routine that accepts new connections and spins them
// off into goroutine handlers.
func (p *ProxyListener) goAcceptRoutine() {
	defer p.waitGroup.Done()
	for !p.stop {
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
			p.protocolErrorLog(fmt.Errorf(
				"Error setting deadline on the socket: %s", err))
			conn.Close()
			return
		}
	}

	// We need to do an unbuffered read since we don't want to over
	// read and start consuming the initial HTTP connection.
	proxyLine := make([]byte, 107)
	proxyLineLen := 0

	// First we need to ensure that the text starts with the expected
	// PROXY line. If not then we can close the session as we would have
	// already consumed bytes which will screw up the underlying protocol
	// for any receiver.
	expect := []byte("PROXY ")
	for proxyLineLen = 0; proxyLineLen < len(expect); proxyLineLen++ {
		buffer := proxyLine[proxyLineLen : len(expect)-proxyLineLen]
		if n, err := conn.Read(buffer); err != nil {
			if err == io.EOF {
				p.protocolErrorLog(fmt.Errorf(
					"TCP session closed prior to PROXY line."))
			} else {
				p.protocolErrorLog(fmt.Errorf(
					"TCP error reading from connection: %s", err))
			}
			conn.Close()
			return
		} else {
			proxyLineLen += int(n)
		}
	}
	if !bytes.Equal(proxyLine[0:proxyLineLen-1], expect) {
		p.protocolErrorLog(fmt.Errorf(
			"TCP session did not start with '" + string(expect) + "'"))
		conn.Close()
		return
	}

	// Loop through the rest of the first line from the socket.
	for i := proxyLineLen; ; i++ {
		if i >= len(proxyLine) {
			p.protocolErrorLog(fmt.Errorf("PROXY line malformed (too long)"))
			conn.Close()
			return
		} else if n, err := conn.Read(proxyLine[i : i+1]); err != nil {
			p.protocolErrorLog(fmt.Errorf("Error reading PROXY line: %s", err))
			conn.Close()
			return
		} else if n != 1 {
			p.protocolErrorLog(fmt.Errorf(
				"Invalid number of bytes read from socket: %d", n))
			conn.Close()
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
			p.protocolErrorLog(fmt.Errorf(
				"PROXY line is malformed, incorrect number of elements: %d",
				len(parts)))
			conn.Close()
			return
		}
		port, err := strconv.ParseInt(string(parts[4]), 10, 32)
		if err != nil {
			p.protocolErrorLog(fmt.Errorf(
				"destination port element of the PROXY line is not an int: %s",
				err))
			conn.Close()
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
		p.protocolErrorLog(fmt.Errorf(
			"Error unsetting deadline on the socket: %s", err))
		conn.Close()
		return
	}

	// Lastly, pass conn on to the Accept caller.
	p.acceptChan <- conn
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
	return p.listener.Close()
}

// Ensures that all goroutines associated with this listener are shutdown and
// closed out.
func (p *ProxyListener) Stop() {
	p.stop = true
	for range p.errorChan {
	}
	for range p.acceptChan {
	}
	p.listener.Close()
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
