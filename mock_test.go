package netproxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"time"
)

type acceptResponse struct {
	Conn      net.Conn
	Error     error
	NotBefore time.Time
}

// ListenerMock is used in several places in order to allow us to mock out
// various calls and simulate all sorts of failures for testing.
type ListenerMock struct {
	AcceptReplies []acceptResponse
	AcceptIndex   int
	RawAddr       net.Addr
	CloseError    error
	Closed        bool
}

func (l *ListenerMock) Accept() (net.Conn, error) {
	addr := l.RawAddr
	if addr == nil {
		addr = &net.IPAddr{IP: net.IP([]byte{1, 2, 3, 4}), Zone: ""}
	}
	if l.AcceptIndex >= len(l.AcceptReplies) {
		return nil, &net.OpError{
			Op:     "The socket is closed.",
			Net:    addr.Network(),
			Source: nil,
			Addr:   addr,
			Err:    fmt.Errorf("The socket is closed."),
		}
	}
	if w := l.AcceptReplies[l.AcceptIndex].NotBefore.Sub(time.Now()); w > 0 {
		time.Sleep(w)
	}
	conn := l.AcceptReplies[l.AcceptIndex].Conn
	err := l.AcceptReplies[l.AcceptIndex].Error
	l.AcceptIndex++
	return conn, err
}

func (l *ListenerMock) Close() error {
	l.Closed = true
	return l.CloseError
}

func (l *ListenerMock) Addr() net.Addr {
	if l.RawAddr == nil {
		return &net.IPAddr{IP: net.IP([]byte{1, 2, 3, 4}), Zone: ""}
	} else {
		return l.RawAddr
	}
}

// For tracking replies that should be sent out via read()
type readResponse struct {
	NotBefore time.Time
	Data      []byte
	Error     error
}

// ConnMock is used to simulate a net.Conn for testing. It allows us to
// mock out all sorts of calls so we can simulate various connection
// states.
type ConnMock struct {
	ReadReplies     []readResponse
	ReadIndex       int
	OutputBuffer    *bufio.Writer
	IsClosed        bool
	RawLocalAddr    net.Addr
	RawRemoteAddr   net.Addr
	ReadDeadline    time.Time
	DeadlineErrors  []error
	DeadlineCounter int
}

func (c *ConnMock) Close() error {
	c.IsClosed = true
	return nil
}

func (c *ConnMock) LocalAddr() net.Addr {
	return c.RawLocalAddr
}

func (c *ConnMock) Read(b []byte) (int, error) {
	if c.ReadIndex >= len(c.ReadReplies) {
		return 0, io.EOF
	}
	if w := c.ReadReplies[c.ReadIndex].NotBefore.Sub(time.Now()); w > 0 {
		time.Sleep(w)
	}
	copy(b, c.ReadReplies[c.ReadIndex].Data)
	n := len(c.ReadReplies[c.ReadIndex].Data)
	err := c.ReadReplies[c.ReadIndex].Error
	c.ReadIndex++
	return n, err
}

func (c *ConnMock) RemoteAddr() net.Addr {
	return c.RawRemoteAddr
}

func (c *ConnMock) Write(b []byte) (int, error) {
	return c.OutputBuffer.Write(b)
}

func (c *ConnMock) SetDeadline(t time.Time) error {
	if c.DeadlineErrors != nil && len(c.DeadlineErrors) > 0 {
		err := c.DeadlineErrors[0]
		if len(c.DeadlineErrors) == 1 {
			c.DeadlineErrors = nil
		} else {
			c.DeadlineErrors = c.DeadlineErrors[1:]
		}
		return err
	}
	c.ReadDeadline = t
	return nil
}

func (c *ConnMock) SetReadDeadline(t time.Time) error {
	return c.SetDeadline(t)
}

func (c *ConnMock) SetWriteDeadline(t time.Time) error {
	return c.SetDeadline(t)
}

// Mocks out net.Addr
type AddrMock struct {
	network string
	str     string
}

func (a *AddrMock) Network() string {
	return a.network
}

func (a *AddrMock) String() string {
	return a.str
}

// A simple helper that takes a byte array and makes it into a readReplies
// array for the ListenerMock.
func makeReplies(b []byte, raw ...[]byte) []readResponse {
	rr := make([]readResponse, 0, len(b)+2+len(raw))
	rr = append(rr, readResponse{Data: []byte("P")})
	rr = append(rr, readResponse{Data: []byte("ROXY ")})
	for _, data := range b {
		rr = append(rr, readResponse{Data: []byte{data}})
	}
	for _, data := range raw {
		rr = append(rr, readResponse{Data: data})
	}
	return rr
}
