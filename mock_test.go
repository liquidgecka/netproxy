package netproxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

type acceptResponse struct {
	Conn  net.Conn
	Error error
}

// ListenerMock simulates a [net.Listener] for unit testing.
type ListenerMock struct {
	AcceptReplies []acceptResponse
	AcceptIndex   int
	RawAddr       net.Addr
	CloseError    error
	Closed        bool
	sync.Mutex
}

func (l *ListenerMock) Accept() (net.Conn, error) {
	l.Lock()
	defer l.Unlock()
	addr := l.RawAddr
	if addr == nil {
		addr = &net.IPAddr{IP: net.IP{1, 2, 3, 4}}
	}
	if l.AcceptIndex >= len(l.AcceptReplies) {
		return nil, &net.OpError{
			Op:   "accept",
			Net:  addr.Network(),
			Addr: addr,
			Err:  errors.New("socket is closed"),
		}
	}
	reply := l.AcceptReplies[l.AcceptIndex]
	l.AcceptIndex++
	return reply.Conn, reply.Error
}

func (l *ListenerMock) Close() error {
	l.Lock()
	defer l.Unlock()
	l.Closed = true
	return l.CloseError
}

func (l *ListenerMock) Addr() net.Addr {
	l.Lock()
	defer l.Unlock()
	if l.RawAddr == nil {
		return &net.IPAddr{IP: net.IP{1, 2, 3, 4}}
	}
	return l.RawAddr
}

type readResponse struct {
	Data  []byte
	Error error
}

// ConnMock simulates a [net.Conn] for unit testing.
type ConnMock struct {
	ReadReplies    []readResponse
	ReadIndex      int
	OutputBuffer   bytes.Buffer
	IsClosed       bool
	RawLocalAddr   net.Addr
	RawRemoteAddr  net.Addr
	ReadDeadline   time.Time
	DeadlineErrors []error
	sync.Mutex
}

func (c *ConnMock) Close() error {
	c.Lock()
	defer c.Unlock()
	c.IsClosed = true
	return nil
}

func (c *ConnMock) LocalAddr() net.Addr {
	c.Lock()
	defer c.Unlock()
	return c.RawLocalAddr
}

func (c *ConnMock) Read(b []byte) (int, error) {
	c.Lock()
	defer c.Unlock()
	if c.ReadIndex >= len(c.ReadReplies) {
		return 0, io.EOF
	}
	reply := &c.ReadReplies[c.ReadIndex]
	n := copy(b, reply.Data)
	if n < len(reply.Data) {
		reply.Data = reply.Data[n:]
		return n, nil
	}
	c.ReadIndex++
	return n, reply.Error
}

func (c *ConnMock) RemoteAddr() net.Addr {
	c.Lock()
	defer c.Unlock()
	return c.RawRemoteAddr
}

func (c *ConnMock) Write(b []byte) (int, error) {
	c.Lock()
	defer c.Unlock()
	return c.OutputBuffer.Write(b)
}

func (c *ConnMock) setDeadlineLocked(t time.Time) error {
	if len(c.DeadlineErrors) > 0 {
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

func (c *ConnMock) SetDeadline(t time.Time) error {
	c.Lock()
	defer c.Unlock()
	return c.setDeadlineLocked(t)
}

func (c *ConnMock) SetReadDeadline(t time.Time) error {
	c.Lock()
	defer c.Unlock()
	return c.setDeadlineLocked(t)
}

func (c *ConnMock) SetWriteDeadline(t time.Time) error {
	c.Lock()
	defer c.Unlock()
	return c.setDeadlineLocked(t)
}

// AddrMock simulates a [net.Addr] for unit testing.
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

func makeRawReplies(chunks ...[]byte) []readResponse {
	rr := make([]readResponse, 0, len(chunks))
	for _, chunk := range chunks {
		rr = append(rr, readResponse{Data: chunk})
	}
	return rr
}

func makeV1Replies(body []byte, trailing ...[]byte) []readResponse {
	rr := make([]readResponse, 0, len(body)+2+len(trailing))
	rr = append(rr, readResponse{Data: []byte("P")})
	rr = append(rr, readResponse{Data: []byte("ROXY ")})
	for _, b := range body {
		rr = append(rr, readResponse{Data: []byte{b}})
	}
	for _, chunk := range trailing {
		rr = append(rr, readResponse{Data: chunk})
	}
	return rr
}

func makeV2Header(verCmd, famProto byte, payloadLen uint16) []byte {
	hdr := make([]byte, 16)
	copy(hdr[:12], v2Signature[:])
	hdr[12] = verCmd
	hdr[13] = famProto
	binary.BigEndian.PutUint16(hdr[14:16], payloadLen)
	return hdr
}
