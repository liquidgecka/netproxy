package netproxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func newTestProxyListener(l net.Listener, errCh chan<- error) *ProxyListener {
	return &ProxyListener{
		listener:   l,
		acceptChan: make(chan net.Conn, acceptQueueSize),
		errorChan:  make(chan error),
		done:       make(chan struct{}),
		pending:    make(map[net.Conn]struct{}),
		ProtocolError: func(err error) {
			if errCh != nil {
				errCh <- err
			}
		},
		ProtocolDeadline: time.Second,
	}
}

func expectProtocolError(t *testing.T, conn *ConnMock, wantSubstr string) {
	t.Helper()
	errCh := make(chan error, 1)
	pl := newTestProxyListener(&ListenerMock{
		AcceptReplies: []acceptResponse{{Conn: conn}},
	}, errCh)
	pl.acceptWG.Go(pl.acceptLoop)
	defer pl.Stop()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected non-nil protocol error")
		}
		if !strings.Contains(err.Error(), wantSubstr) {
			t.Fatalf("protocol error %q does not contain %q", err.Error(), wantSubstr)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for protocol error containing %q", wantSubstr)
	}
}

func TestListen(t *testing.T) {
	l, err := Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	if l == nil {
		t.Fatal("Listen() returned nil listener")
	}
	pl, ok := l.(*ProxyListener)
	if !ok {
		t.Fatalf("Listen() returned %T, want *ProxyListener", l)
	}
	if pl.Addr() == nil {
		t.Fatal("Addr() returned nil")
	}
	pl.Stop()
}

func TestListen_Error(t *testing.T) {
	l, err := Listen("NOT_VALID", ":::")
	if l != nil {
		t.Fatalf("Listen() returned %v, want nil", l)
	}
	if err == nil {
		t.Fatal("Listen() error = nil, want error")
	}
}

func TestListenWrapper_StartAndStop(t *testing.T) {
	rawListener := &ListenerMock{}
	pl := ListenWrapper(
		rawListener,
		nil,
		WithProtocolDeadline(2*time.Second),
		WithProtocolError(func(error) {}),
	)
	if pl == nil {
		t.Fatal("ListenWrapper() returned nil")
	}
	if pl.ProtocolDeadline != 2*time.Second {
		t.Fatalf("ProtocolDeadline = %v, want 2s", pl.ProtocolDeadline)
	}
	if pl.ProtocolError == nil {
		t.Fatal("ProtocolError = nil, want non-nil")
	}

	pl.Stop()
	pl.Stop() // verify idempotency

	if !rawListener.Closed {
		t.Fatal("underlying listener was not closed")
	}
	if _, err := pl.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept() after Stop() error = %v, want %v", err, net.ErrClosed)
	}
}

func TestProxyListener_reportProtocolError(t *testing.T) {
	var logged []error
	pl := ProxyListener{
		ProtocolError: func(err error) {
			logged = append(logged, err)
		},
	}

	conn1 := &ConnMock{}
	pl.reportProtocolError(conn1, errors.New("test1"))
	if !conn1.IsClosed {
		t.Fatal("conn1 was not closed")
	}

	conn2 := &ConnMock{}
	pl.ProtocolError = nil
	pl.reportProtocolError(conn2, errors.New("test2"))
	if !conn2.IsClosed {
		t.Fatal("conn2 was not closed")
	}

	conn3 := &ConnMock{}
	pl.ProtocolError = func(err error) {
		logged = append(logged, err)
	}
	pl.closed.Store(true)
	pl.reportProtocolError(conn3, errors.New("test3"))
	if !conn3.IsClosed {
		t.Fatal("conn3 was not closed")
	}

	if len(logged) != 1 {
		t.Fatalf("logged %d errors (%v), want 1", len(logged), logged)
	}
}

func TestProxyListener_ReadHeaderErrors(t *testing.T) {
	v1TooLong := strings.Repeat("X", maxV1LineLen)
	v1ExactCRAtEnd := strings.Repeat("X", maxV1LineLen-len("PROXY ")-1) + "\r\n"

	tests := []struct {
		name       string
		conn       *ConnMock
		wantSubstr string
	}{
		{
			name: "deadline_set_error",
			conn: &ConnMock{
				DeadlineErrors: []error{errors.New("EXPECTED")},
			},
			wantSubstr: "setting protocol deadline: EXPECTED",
		},
		{
			name:       "closed_before_first_byte",
			conn:       &ConnMock{},
			wantSubstr: "connection closed before first byte",
		},
		{
			name: "first_byte_read_error",
			conn: &ConnMock{
				ReadReplies: []readResponse{{Error: errors.New("EXPECTED")}},
			},
			wantSubstr: "reading first byte: EXPECTED",
		},
		{
			name: "invalid_first_byte",
			conn: &ConnMock{
				ReadReplies: makeRawReplies([]byte("X")),
			},
			wantSubstr: "invalid first byte: 88",
		},
		{
			name: "v1_closed_prior_to_prefix",
			conn: &ConnMock{
				ReadReplies: makeRawReplies([]byte("P")),
			},
			wantSubstr: "connection closed prior to PROXY prefix",
		},
		{
			name: "v1_prefix_read_error",
			conn: &ConnMock{
				ReadReplies: []readResponse{
					{Data: []byte("P")},
					{Error: errors.New("EXPECTED")},
				},
			},
			wantSubstr: "reading PROXY prefix: EXPECTED",
		},
		{
			name: "v1_non_proxy_start",
			conn: &ConnMock{
				ReadReplies: makeRawReplies([]byte("P"), []byte("WTFWTF")),
			},
			wantSubstr: "header did not start with 'PROXY '",
		},
		{
			name: "v1_line_too_long_no_cr",
			conn: &ConnMock{
				ReadReplies: makeV1Replies([]byte(v1TooLong)),
			},
			wantSubstr: "exceeds maximum length",
		},
		{
			name: "v1_line_too_long_cr_at_107th_byte",
			conn: &ConnMock{
				ReadReplies: makeV1Replies([]byte(v1ExactCRAtEnd)),
			},
			wantSubstr: "exceeds maximum length",
		},
		{
			name: "v1_line_read_error",
			conn: &ConnMock{
				ReadReplies: []readResponse{
					{Data: []byte("P")},
					{Data: []byte("ROXY ")},
					{Error: errors.New("EXPECTED")},
				},
			},
			wantSubstr: "reading PROXY line: EXPECTED",
		},
		{
			name: "v1_crlf_read_error",
			conn: &ConnMock{
				ReadReplies: []readResponse{
					{Data: []byte("P")},
					{Data: []byte("ROXY ")},
					{Data: []byte("U")},
					{Data: []byte("\r")},
					{Error: errors.New("EXPECTED")},
				},
			},
			wantSubstr: "reading PROXY line CRLF: EXPECTED",
		},
		{
			name: "v1_bare_lf",
			conn: &ConnMock{
				ReadReplies: makeV1Replies([]byte("TCP4 1.1.1.1 2.2.2.2 1 2\n")),
			},
			wantSubstr: "bare LF without CR",
		},
		{
			name: "v1_cr_not_followed_by_lf",
			conn: &ConnMock{
				ReadReplies: makeV1Replies([]byte("TCP4 1.1.1.1 2.2.2.2 1 2\rX")),
			},
			wantSubstr: "CR not followed by LF",
		},
		{
			name: "v1_wrong_number_of_elements",
			conn: &ConnMock{
				ReadReplies: makeV1Replies([]byte("1 2 3 4 5 6 7 8 9\r\n")),
			},
			wantSubstr: "incorrect number of elements: 10",
		},
		{
			name: "v1_invalid_protocol",
			conn: &ConnMock{
				ReadReplies: makeV1Replies([]byte("XXX 1 2 3 4\r\n")),
			},
			wantSubstr: `invalid PROXY protocol "XXX"`,
		},
		{
			name: "v1_invalid_src_ip",
			conn: &ConnMock{
				ReadReplies: makeV1Replies([]byte("TCP4 bad 2.2.2.2 1 2\r\n")),
			},
			wantSubstr: "invalid source IP",
		},
		{
			name: "v1_invalid_dst_ip",
			conn: &ConnMock{
				ReadReplies: makeV1Replies([]byte("TCP4 1.1.1.1 bad 1 2\r\n")),
			},
			wantSubstr: "invalid destination IP",
		},
		{
			name: "v1_tcp4_with_ipv6_addr",
			conn: &ConnMock{
				ReadReplies: makeV1Replies([]byte("TCP4 ::1 ::2 1 2\r\n")),
			},
			wantSubstr: "protocol TCP4 requires IPv4 addresses",
		},
		{
			name: "v1_tcp6_with_ipv4_addr",
			conn: &ConnMock{
				ReadReplies: makeV1Replies([]byte("TCP6 1.1.1.1 2.2.2.2 1 2\r\n")),
			},
			wantSubstr: "protocol TCP6 requires IPv6 addresses",
		},
		{
			name: "v1_tcp6_with_zone",
			conn: &ConnMock{
				ReadReplies: makeV1Replies([]byte("TCP6 fe80::1%eth0 fe80::2 1 2\r\n")),
			},
			wantSubstr: "protocol TCP6 requires IPv6 addresses",
		},
		{
			name: "v1_bad_source_port",
			conn: &ConnMock{
				ReadReplies: makeV1Replies([]byte("TCP4 1.1.1.1 2.2.2.2 X 1\r\n")),
			},
			wantSubstr: "invalid source port",
		},
		{
			name: "v1_source_port_out_of_range",
			conn: &ConnMock{
				ReadReplies: makeV1Replies([]byte("TCP4 1.1.1.1 2.2.2.2 70000 1\r\n")),
			},
			wantSubstr: "invalid source port",
		},
		{
			name: "v1_bad_dest_port",
			conn: &ConnMock{
				ReadReplies: makeV1Replies([]byte("TCP4 1.1.1.1 2.2.2.2 1 X\r\n")),
			},
			wantSubstr: "invalid destination port",
		},
		{
			name: "v1_clear_deadline_error",
			conn: &ConnMock{
				DeadlineErrors: []error{nil, errors.New("EXPECTED")},
				ReadReplies:    makeV1Replies([]byte("TCP4 1.1.1.1 2.2.2.2 1 2\r\n")),
			},
			wantSubstr: "clearing protocol deadline: EXPECTED",
		},
		{
			name: "v2_early_header_eof",
			conn: &ConnMock{
				ReadReplies: makeRawReplies([]byte{0x0D}),
			},
			wantSubstr: "reading binary header",
		},
		{
			name: "v2_header_read_error",
			conn: &ConnMock{
				ReadReplies: []readResponse{
					{Data: []byte{0x0D}},
					{Error: errors.New("EXPECTED")},
				},
			},
			wantSubstr: "reading binary header: EXPECTED",
		},
		{
			name: "v2_invalid_signature_zeros",
			conn: &ConnMock{
				ReadReplies: makeRawReplies([]byte{0x0D}, bytes.Repeat([]byte{0}, 15)),
			},
			wantSubstr: "binary signature mismatch",
		},
		{
			name: "v2_old_buggy_signature_rejected",
			conn: &ConnMock{
				ReadReplies: makeRawReplies(
					[]byte{0x0D},
					[]byte{
						0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
						0x51, 0x55, 0x49, 0x54, 0x10,
					},
					[]byte{0x20, 0x00, 0x00, 0x00},
				),
			},
			wantSubstr: "binary signature mismatch",
		},
		{
			name: "v2_invalid_version",
			conn: &ConnMock{
				ReadReplies: makeRawReplies(
					[]byte{0x0D},
					v2Signature[1:],
					[]byte{0xF0, 0x00, 0x00, 0x00},
				),
			},
			wantSubstr: "unsupported binary protocol version: 15",
		},
		{
			name: "v2_invalid_command",
			conn: &ConnMock{
				ReadReplies: makeRawReplies(
					[]byte{0x0D},
					v2Signature[1:],
					[]byte{0x2F, 0x00, 0x00, 0x00},
				),
			},
			wantSubstr: "invalid binary command: 15",
		},
		{
			name: "v2_invalid_family",
			conn: &ConnMock{
				ReadReplies: makeRawReplies(
					[]byte{0x0D},
					v2Signature[1:],
					[]byte{0x20, 0xF0, 0x00, 0x00},
				),
			},
			wantSubstr: "invalid binary address family: 15",
		},
		{
			name: "v2_invalid_protocol",
			conn: &ConnMock{
				ReadReplies: makeRawReplies(
					[]byte{0x0D},
					v2Signature[1:],
					[]byte{0x20, 0x0F, 0x00, 0x00},
				),
			},
			wantSubstr: "invalid binary transport protocol: 15",
		},
		{
			name: "v2_payload_read_error",
			conn: &ConnMock{
				ReadReplies: []readResponse{
					{Data: []byte{0x0D}},
					{Data: v2Signature[1:]},
					{Data: []byte{0x20, 0x00, 0x00, 0x01}},
					{Error: errors.New("EXPECTED")},
				},
			},
			wantSubstr: "reading binary address payload: EXPECTED",
		},
		{
			name: "v2_clear_deadline_error",
			conn: &ConnMock{
				DeadlineErrors: []error{nil, errors.New("EXPECTED")},
				ReadReplies: makeRawReplies(
					[]byte{0x0D},
					v2Signature[1:],
					[]byte{0x20, 0x00, 0x00, 0x00},
				),
			},
			wantSubstr: "clearing protocol deadline: EXPECTED",
		},
		{
			name: "v2_ipv4_tcp_too_short",
			conn: &ConnMock{
				ReadReplies: makeRawReplies(makeV2Header(0x21, 0x11, 0)),
			},
			wantSubstr: "invalid IPv4 proxy address length: got 0, want at least 12",
		},
		{
			name: "v2_ipv4_udp_too_short",
			conn: &ConnMock{
				ReadReplies: makeRawReplies(makeV2Header(0x21, 0x12, 0)),
			},
			wantSubstr: "invalid IPv4 proxy address length: got 0, want at least 12",
		},
		{
			name: "v2_ipv6_tcp_too_short",
			conn: &ConnMock{
				ReadReplies: makeRawReplies(makeV2Header(0x21, 0x21, 0)),
			},
			wantSubstr: "invalid IPv6 proxy address length: got 0, want at least 36",
		},
		{
			name: "v2_ipv6_udp_too_short",
			conn: &ConnMock{
				ReadReplies: makeRawReplies(makeV2Header(0x21, 0x22, 0)),
			},
			wantSubstr: "invalid IPv6 proxy address length: got 0, want at least 36",
		},
		{
			name: "v2_unix_too_short",
			conn: &ConnMock{
				ReadReplies: makeRawReplies(makeV2Header(0x21, 0x31, 0)),
			},
			wantSubstr: "invalid UNIX proxy address length: got 0, want at least 216",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			expectProtocolError(t, tc.conn, tc.wantSubstr)
		})
	}
}

func TestParseVersion1Line_Direct(t *testing.T) {
	if _, err := parseVersion1Line([]byte("PROXY")); err == nil {
		t.Fatal("parseVersion1Line(\"PROXY\") expected error, got nil")
	}
}

func TestParseBinaryAddr_UnsupportedCombination(t *testing.T) {
	if _, err := parseBinaryAddr(0x0F, protocolStream, nil); err == nil {
		t.Fatal("parseBinaryAddr() expected error for invalid family, got nil")
	}
	if _, err := parseBinaryAddr(familyInet, 0x0F, make([]byte, v2IPv4AddrLen)); err == nil {
		t.Fatal("parseBinaryAddr() expected error for invalid protocol, got nil")
	}
}

func TestProxyListener_ReadVersion1_Success(t *testing.T) {
	maxSrcIP := "1111:2222:3333:4444:5555:6666:7777:8888"
	maxDstIP := "aaaa:bbbb:cccc:dddd:eeee:ffff:0000:1111"
	// "PROXY TCP6 " (11) + 39 + 1 + 39 + 1 + "65535 65535" (11) + "\r\n" (2) = 104 bytes;
	// pad destination or use full canonical form; 107 is max line length so test exact 107 via UNKNOWN.
	exact107Unknown := "UNKNOWN " + strings.Repeat("A", maxV1LineLen-len("PROXY UNKNOWN \r\n")) + "\r\n"

	tests := []struct {
		name         string
		body         []byte
		wantAddr     string
		wantTCP      bool
		wantNetMatch string
	}{
		{
			name:     "tcp4",
			body:     []byte("TCP4 1.1.1.1 2.2.2.2 1 2\r\n"),
			wantAddr: "1.1.1.1:1",
			wantTCP:  true,
		},
		{
			name:     "tcp6",
			body:     []byte("TCP6 " + maxSrcIP + " " + maxDstIP + " 65535 443\r\n"),
			wantAddr: "[" + maxSrcIP + "]:65535",
			wantTCP:  true,
		},
		{
			name:         "unknown_bare",
			body:         []byte("UNKNOWN\r\n"),
			wantAddr:     "9.9.9.9:9999",
			wantNetMatch: "net",
		},
		{
			name:         "unknown_exact_107_bytes",
			body:         []byte(exact107Unknown),
			wantAddr:     "9.9.9.9:9999",
			wantNetMatch: "net",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rawConn := &ConnMock{
				RawRemoteAddr: &AddrMock{network: "net", str: "9.9.9.9:9999"},
				ReadReplies:   makeV1Replies(tc.body, []byte("OK")),
			}
			errCh := make(chan error, 1)
			pl := newTestProxyListener(&ListenerMock{
				AcceptReplies: []acceptResponse{{Conn: rawConn}},
			}, errCh)
			pl.acceptWG.Go(pl.acceptLoop)
			defer pl.Stop()

			select {
			case conn := <-pl.acceptChan:
				if conn.RemoteAddr().String() != tc.wantAddr {
					t.Fatalf("RemoteAddr() = %q, want %q", conn.RemoteAddr().String(), tc.wantAddr)
				}
				if tc.wantTCP {
					if _, ok := conn.RemoteAddr().(*net.TCPAddr); !ok {
						t.Fatalf("RemoteAddr() type = %T, want *net.TCPAddr", conn.RemoteAddr())
					}
					unwrapper, ok := conn.(interface{ NetConn() net.Conn })
					if !ok || unwrapper.NetConn() != rawConn {
						t.Fatal("NetConn() did not return underlying connection")
					}
				}
				if tc.wantNetMatch != "" && conn.RemoteAddr().Network() != tc.wantNetMatch {
					t.Fatalf("Network() = %q, want %q", conn.RemoteAddr().Network(), tc.wantNetMatch)
				}

				buf := make([]byte, 2)
				if _, err := io.ReadFull(conn, buf); err != nil {
					t.Fatalf("ReadFull() error = %v", err)
				}
				if string(buf) != "OK" {
					t.Fatalf("payload = %q, want %q", string(buf), "OK")
				}
			case err := <-errCh:
				t.Fatalf("unexpected protocol error: %v", err)
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for accepted connection")
			}
		})
	}
}

func TestProxyListener_ReadBinary_Success(t *testing.T) {
	ipv6Src := []byte{
		0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
		0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F,
	}
	ipv6Dst := []byte{
		0x0F, 0x0E, 0x0D, 0x0C, 0x0B, 0x0A, 0x09, 0x08,
		0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01, 0x00,
	}
	ports := []byte{0x09, 0x0A, 0x0B, 0x0C} // src=2314, dst=2828

	ipv4Payload := []byte{1, 2, 3, 4, 5, 6, 7, 8, 0x09, 0x0A, 0x0B, 0x0C}
	ipv6Payload := append(append(append([]byte(nil), ipv6Src...), ipv6Dst...), ports...)

	unixShortPayload := make([]byte, v2UnixAddrLen)
	copy(unixShortPayload[:v2UnixPathLen], "/var/run/proxy.sock")

	unixFullPath := strings.Repeat("a", v2UnixPathLen)
	unixFullPayloadWithTLV := make([]byte, v2UnixAddrLen+5)
	copy(unixFullPayloadWithTLV[:v2UnixPathLen], unixFullPath)
	copy(unixFullPayloadWithTLV[v2UnixAddrLen:], []byte{0x01, 0x00, 0x02, 'h', 'i'})

	tests := []struct {
		name     string
		chunks   [][]byte
		wantAddr string
		checkTyp func(t *testing.T, addr net.Addr)
	}{
		{
			name:     "local_unspec",
			chunks:   [][]byte{makeV2Header(0x20, 0x00, 0), []byte("OK")},
			wantAddr: "0.0.0.0:0",
		},
		{
			name:     "local_with_payload_ignored",
			chunks:   [][]byte{makeV2Header(0x20, 0x11, 12), ipv4Payload, []byte("OK")},
			wantAddr: "0.0.0.0:0",
		},
		{
			name:     "proxy_unspec_family_with_tlv",
			chunks:   [][]byte{makeV2Header(0x21, 0x01, 4), {1, 2, 3, 4}, []byte("OK")},
			wantAddr: "0.0.0.0:0",
		},
		{
			name:     "proxy_unspec_protocol",
			chunks:   [][]byte{makeV2Header(0x21, 0x10, 0), []byte("OK")},
			wantAddr: "0.0.0.0:0",
		},
		{
			name:     "tcp_over_ipv4",
			chunks:   [][]byte{makeV2Header(0x21, 0x11, 12), ipv4Payload, []byte("OK")},
			wantAddr: "1.2.3.4:2314",
			checkTyp: func(t *testing.T, addr net.Addr) {
				if _, ok := addr.(*net.TCPAddr); !ok {
					t.Fatalf("addr type = %T, want *net.TCPAddr", addr)
				}
			},
		},
		{
			name: "tcp_over_ipv4_with_tlv",
			chunks: [][]byte{
				makeV2Header(0x21, 0x11, 16),
				ipv4Payload,
				{0x01, 0x00, 0x01, 0xFF},
				[]byte("OK"),
			},
			wantAddr: "1.2.3.4:2314",
			checkTyp: func(t *testing.T, addr net.Addr) {
				if _, ok := addr.(*net.TCPAddr); !ok {
					t.Fatalf("addr type = %T, want *net.TCPAddr", addr)
				}
			},
		},
		{
			name:     "udp_over_ipv4",
			chunks:   [][]byte{makeV2Header(0x21, 0x12, 12), ipv4Payload, []byte("OK")},
			wantAddr: "1.2.3.4:2314",
			checkTyp: func(t *testing.T, addr net.Addr) {
				if _, ok := addr.(*net.UDPAddr); !ok {
					t.Fatalf("addr type = %T, want *net.UDPAddr", addr)
				}
			},
		},
		{
			name:     "tcp_over_ipv6",
			chunks:   [][]byte{makeV2Header(0x21, 0x21, 36), ipv6Payload, []byte("OK")},
			wantAddr: "[1:203:405:607:809:a0b:c0d:e0f]:2314",
			checkTyp: func(t *testing.T, addr net.Addr) {
				if _, ok := addr.(*net.TCPAddr); !ok {
					t.Fatalf("addr type = %T, want *net.TCPAddr", addr)
				}
			},
		},
		{
			name:     "udp_over_ipv6",
			chunks:   [][]byte{makeV2Header(0x21, 0x22, 36), ipv6Payload, []byte("OK")},
			wantAddr: "[1:203:405:607:809:a0b:c0d:e0f]:2314",
			checkTyp: func(t *testing.T, addr net.Addr) {
				if _, ok := addr.(*net.UDPAddr); !ok {
					t.Fatalf("addr type = %T, want *net.UDPAddr", addr)
				}
			},
		},
		{
			name:     "unix_stream",
			chunks:   [][]byte{makeV2Header(0x21, 0x31, v2UnixAddrLen), unixShortPayload, []byte("OK")},
			wantAddr: "/var/run/proxy.sock",
			checkTyp: func(t *testing.T, addr net.Addr) {
				ua, ok := addr.(*net.UnixAddr)
				if !ok {
					t.Fatalf("addr type = %T, want *net.UnixAddr", addr)
				}
				if ua.Network() != "unix" {
					t.Fatalf("Network() = %q, want unix", ua.Network())
				}
			},
		},
		{
			name: "unix_dgram_full_path_with_tlv",
			chunks: [][]byte{
				makeV2Header(0x21, 0x32, uint16(len(unixFullPayloadWithTLV))),
				unixFullPayloadWithTLV,
				[]byte("OK"),
			},
			wantAddr: unixFullPath,
			checkTyp: func(t *testing.T, addr net.Addr) {
				ua, ok := addr.(*net.UnixAddr)
				if !ok {
					t.Fatalf("addr type = %T, want *net.UnixAddr", addr)
				}
				if ua.Network() != "unixgram" {
					t.Fatalf("Network() = %q, want unixgram", ua.Network())
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rawConn := &ConnMock{
				RawRemoteAddr: &AddrMock{network: "net", str: "0.0.0.0:0"},
				ReadReplies:   makeRawReplies(tc.chunks...),
			}
			errCh := make(chan error, 1)
			pl := newTestProxyListener(&ListenerMock{
				AcceptReplies: []acceptResponse{{Conn: rawConn}},
			}, errCh)
			pl.acceptWG.Go(pl.acceptLoop)
			defer pl.Stop()

			select {
			case conn := <-pl.acceptChan:
				if conn.RemoteAddr().String() != tc.wantAddr {
					t.Fatalf("RemoteAddr() = %q, want %q", conn.RemoteAddr().String(), tc.wantAddr)
				}
				if tc.checkTyp != nil {
					tc.checkTyp(t, conn.RemoteAddr())
				}
				buf := make([]byte, 2)
				if _, err := io.ReadFull(conn, buf); err != nil {
					t.Fatalf("ReadFull() error = %v", err)
				}
				if string(buf) != "OK" {
					t.Fatalf("payload = %q, want %q", string(buf), "OK")
				}
			case err := <-errCh:
				t.Fatalf("unexpected protocol error: %v", err)
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for accepted connection")
			}
		})
	}
}

func TestProxyListener_Accept_Error(t *testing.T) {
	p := &ProxyListener{
		errorChan:  make(chan error, 1),
		acceptChan: make(chan net.Conn, 1),
		done:       make(chan struct{}),
	}
	p.errorChan <- errors.New("EXPECTED")
	conn, err := p.Accept()
	if err == nil || err.Error() != "EXPECTED" {
		t.Fatalf("Accept() error = %v, want EXPECTED", err)
	}
	if conn != nil {
		t.Fatalf("Accept() conn = %v, want nil", conn)
	}
}

func TestProxyListener_Accept_Connection(t *testing.T) {
	p := &ProxyListener{
		errorChan:  make(chan error, 1),
		acceptChan: make(chan net.Conn, 1),
		done:       make(chan struct{}),
	}
	p.acceptChan <- &ConnMock{}
	conn, err := p.Accept()
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	if conn == nil {
		t.Fatal("Accept() conn = nil, want non-nil")
	}
}

func TestProxyListener_Accept_UnblocksOnClose(t *testing.T) {
	pl := ListenWrapper(&ListenerMock{
		AcceptReplies: []acceptResponse{{Error: errors.New("transient")}},
	})

	doneCh := make(chan error, 1)
	go func() {
		_, err := pl.Accept()
		doneCh <- err
	}()

	// Allow Accept to consume or wait, then Close.
	time.Sleep(10 * time.Millisecond)
	if err := pl.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	select {
	case err := <-doneCh:
		if err == nil {
			t.Fatal("Accept() returned nil error after Close()")
		}
	case <-time.After(time.Second):
		t.Fatal("Accept() did not unblock after Close()")
	}
	pl.Stop()
}

func TestProxyListener_Addr(t *testing.T) {
	lm := &ListenerMock{
		RawAddr: &AddrMock{network: "xxx", str: "yyy"},
	}
	p := &ProxyListener{listener: lm}
	if got := p.Addr(); got != lm.RawAddr {
		t.Fatalf("Addr() = %v, want %v", got, lm.RawAddr)
	}
}

func TestProxyListener_Close(t *testing.T) {
	wantErr := errors.New("close err")
	lm := &ListenerMock{CloseError: wantErr}
	p := &ProxyListener{listener: lm}
	if err := p.Close(); !errors.Is(err, wantErr) {
		t.Fatalf("Close() error = %v, want %v", err, wantErr)
	}
	if !lm.Closed {
		t.Fatal("underlying listener was not closed")
	}
	if _, err := p.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept() after Close() error = %v, want %v", err, net.ErrClosed)
	}
}

func TestProxyListener_Integration_TCP(t *testing.T) {
	t.Run("v1_header_and_payload", func(t *testing.T) {
		l, err := Listen("tcp", "127.0.0.1:0", WithProtocolDeadline(time.Second))
		if err != nil {
			t.Fatalf("Listen() error = %v", err)
		}
		pl := l.(*ProxyListener)
		defer pl.Stop()

		client, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			t.Fatalf("Dial() error = %v", err)
		}
		defer func() { _ = client.Close() }()

		if _, err := client.Write([]byte("PROXY TCP4 203.0.113.10 198.51.100.20 54321 8080\r\nhello")); err != nil {
			t.Fatalf("Write() error = %v", err)
		}

		srvConn, err := pl.Accept()
		if err != nil {
			t.Fatalf("Accept() error = %v", err)
		}
		defer func() { _ = srvConn.Close() }()

		if got := srvConn.RemoteAddr().String(); got != "203.0.113.10:54321" {
			t.Fatalf("RemoteAddr() = %q, want 203.0.113.10:54321", got)
		}
		buf := make([]byte, 5)
		if _, err := io.ReadFull(srvConn, buf); err != nil {
			t.Fatalf("ReadFull() error = %v", err)
		}
		if string(buf) != "hello" {
			t.Fatalf("payload = %q, want %q", string(buf), "hello")
		}
	})

	t.Run("v2_ipv6_header_and_payload", func(t *testing.T) {
		l, err := Listen("tcp", "127.0.0.1:0", WithProtocolDeadline(time.Second))
		if err != nil {
			t.Fatalf("Listen() error = %v", err)
		}
		pl := l.(*ProxyListener)
		defer pl.Stop()

		client, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			t.Fatalf("Dial() error = %v", err)
		}
		defer func() { _ = client.Close() }()

		pkt := makeV2Header(0x21, 0x21, 36)
		srcIP := net.ParseIP("2001:db8::1").To16()
		dstIP := net.ParseIP("2001:db8::2").To16()
		pkt = append(pkt, srcIP...)
		pkt = append(pkt, dstIP...)
		pkt = binary.BigEndian.AppendUint16(pkt, 4444)
		pkt = binary.BigEndian.AppendUint16(pkt, 8443)
		pkt = append(pkt, []byte("world")...)

		if _, err := client.Write(pkt); err != nil {
			t.Fatalf("Write() error = %v", err)
		}

		srvConn, err := pl.Accept()
		if err != nil {
			t.Fatalf("Accept() error = %v", err)
		}
		defer func() { _ = srvConn.Close() }()

		if got := srvConn.RemoteAddr().String(); got != "[2001:db8::1]:4444" {
			t.Fatalf("RemoteAddr() = %q, want [2001:db8::1]:4444", got)
		}
		buf := make([]byte, 5)
		if _, err := io.ReadFull(srvConn, buf); err != nil {
			t.Fatalf("ReadFull() error = %v", err)
		}
		if string(buf) != "world" {
			t.Fatalf("payload = %q, want %q", string(buf), "world")
		}
	})

	t.Run("protocol_deadline_expires", func(t *testing.T) {
		raw, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("net.Listen() error = %v", err)
		}
		errCh := make(chan error, 1)
		pl := ListenWrapper(
			raw,
			WithProtocolDeadline(20*time.Millisecond),
			WithProtocolError(func(err error) {
				errCh <- err
			}),
		)
		defer pl.Stop()

		client, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			t.Fatalf("Dial() error = %v", err)
		}
		defer func() { _ = client.Close() }()

		select {
		case err := <-errCh:
			if err == nil {
				t.Fatal("expected deadline error, got nil")
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for ProtocolDeadline error")
		}
	})

	t.Run("stop_unblocks_stalled_handshake_and_drains_queue", func(t *testing.T) {
		l, err := Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Listen() error = %v", err)
		}
		pl := l.(*ProxyListener)

		// Connect one client that stalls mid-handshake.
		stalled, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			t.Fatalf("Dial(stalled) error = %v", err)
		}
		defer func() { _ = stalled.Close() }()

		// Connect a second client that completes handshake so it sits in acceptChan.
		queued, err := net.Dial("tcp", pl.Addr().String())
		if err != nil {
			t.Fatalf("Dial(queued) error = %v", err)
		}
		defer func() { _ = queued.Close() }()
		if _, err := queued.Write([]byte("PROXY UNKNOWN\r\n")); err != nil {
			t.Fatalf("Write() error = %v", err)
		}

		// Wait until the queued connection reaches acceptChan.
		deadline := time.Now().Add(time.Second)
		for len(pl.acceptChan) == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}

		stopped := make(chan struct{})
		go func() {
			pl.Stop()
			close(stopped)
		}()

		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("Stop() hung on stalled handshaking connection")
		}

		if _, err := pl.Accept(); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept() after Stop() = %v, want %v", err, net.ErrClosed)
		}
	})
}
