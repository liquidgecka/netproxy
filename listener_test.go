package netproxy

import (
	"bytes"
	"fmt"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestListen(t *testing.T) {
	l, err := Listen("tcp", "")
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	} else if l == nil {
		t.Fatalf("Expected return, got nil.")
	} else if lw, ok := l.(*ProxyListener); !ok {
		t.Fatalf("Listen() returned an invalid type: %t", l)
	} else {
		lw.Stop()
	}
}

func TestListen_Error(t *testing.T) {
	// We can't use a mock here because this calls net.Listen() directly.
	// We induce an error by using an invalid network name that shouldn't
	// ever be valid.
	l, err := Listen("NOT_VALID", ":::")
	if l != nil {
		t.Fatalf("Listen should have returned nil.")
	} else if err == nil {
		t.Fatalf("Listen should have returned an error.")
	}
}

func TestListenWrapper_StartAndStop(t *testing.T) {
	// Keep a counter of how many goroutines existed before we started.
	startingRoutines := runtime.NumGoroutine()

	// Startup a listener. We can use a mock here in order to be absolutely
	// sure that nothing external to us creates an untracked goroutine.
	rawListener := &ListenerMock{}
	l := ListenWrapper(rawListener)
	if l == nil {
		t.Fatalf("ListenWrapper() returned nil")
	}

	// Make sure that the listener can stop.
	l.Stop()

	if rawListener.Closed != true {
		t.Fatalf("Underlying Listener was not closed.")
	} else if runtime.NumGoroutine() != startingRoutines {
		t.Fatalf("The number of goroutines increased after Stop().")
	}
}

func TestProxyListener_protocolErrorLog(t *testing.T) {
	loggedErrors := make([]error, 0, 2)
	pl := ProxyListener{
		ProtocolError: func(err error) {
			loggedErrors = append(loggedErrors, err)
		},
	}
	// Test 1
	conn := &ConnMock{}
	pl.protocolErrorLog(conn, "test1")
	if !conn.IsClosed {
		t.Fatalf("test1: Connection was not closed.")
	}
	conn = &ConnMock{}
	pl.ProtocolError = nil
	pl.protocolErrorLog(conn, "test2")
	if !conn.IsClosed {
		t.Fatalf("test2: Connection was not closed.")
	}

	if len(loggedErrors) != 1 {
		t.Logf("%#v", loggedErrors)
		t.Fatalf("^- len() != 1")
	}
}

func TestProxyListener_goAcceptRoutine(t *testing.T) {
	// Keep a counter of how many goroutines existed before we started.
	startingRoutines := runtime.NumGoroutine()

	// Minimally start a new ListenerMock
	pl := &ProxyListener{
		listener: &ListenerMock{
			AcceptReplies: []acceptResponse{
				acceptResponse{Conn: &ConnMock{}},
			},
		},
		errorChan:  make(chan error),
		acceptChan: make(chan net.Conn),
	}
	pl.waitGroup.Add(1)
	go pl.goAcceptRoutine()

	// We want to make sure it has time to process.
	startTime := time.Now()
	for pl.listener.(*ListenerMock).AcceptIndex != 1 {
		if time.Now().Sub(startTime) > time.Second {
			t.Fatalf("goAcceptRoutine() took too long accepting connections.")
		}
		time.Sleep(time.Millisecond)
	}

	// Read the error off the channel.
	select {
	case err := <-pl.errorChan:
		if err == nil {
			t.Fatalf("goAcceptRoutine() didn't return an expected error.")
		}
	case <-time.NewTimer(time.Second).C:
		t.Fatalf("goAcceptRoutine() didn't report back an error.")
	}

	// Stop the processor.
	pl.Stop()

	// Make sure both channels are closed.
	for range pl.errorChan {
		t.Fatalf("errorChan was not closed()")
	}
	for range pl.acceptChan {
		t.Fatalf("acceptChan was not closed()")
	}

	// Make sure that the goroutine actually exited.
	if runtime.NumGoroutine() != startingRoutines {
		t.Fatalf("The number of goroutines increased after Stop().")
	}
}

func connectionTestWrapper(t *testing.T, conn *ConnMock, expected string) {
	errors := make([]error, 0, 2)
	pl := &ProxyListener{
		listener: &ListenerMock{
			AcceptReplies: []acceptResponse{
				acceptResponse{Conn: conn},
			},
		},
		errorChan:  make(chan error),
		acceptChan: make(chan net.Conn),
		ProtocolError: func(err error) {
			errors = append(errors, err)
		},
		ProtocolDeadline: time.Second,
	}
	pl.waitGroup.Add(1)
	go pl.goAcceptRoutine()
	<-pl.errorChan
	pl.Stop()

	// Make sure the error was received.
	if len(errors) != 1 {
		t.Logf("%#v", errors)
		t.Fatalf("Expected exactly one error.")
	} else if errors[0].Error() != expected {
		t.Fatalf("Unknown error returned: %s", errors[0])
	}
}

func TestProxyListener_goReadProxyRoutine_DeadlineError(t *testing.T) {
	conn := &ConnMock{
		DeadlineErrors: []error{fmt.Errorf("EXPECTED")},
	}
	expected := "Error setting deadline on the socket: EXPECTED"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_goReadProxyRoutine_ClosedEarly(t *testing.T) {
	conn := &ConnMock{}
	expected := "Connection closed before first byte."
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_goReadProxyRoutine_ErrorReading(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Error: fmt.Errorf("EXPECTED"),
			},
		},
	}
	expected := "Error reading first byte: EXPECTED"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_goReadProxyRoutine_InvalidReadBytes(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte("XX"),
			},
		},
	}
	expected := "Invalid number of bytes read: 2"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_goReadProxyRoutine_InvalidFirstByte(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte("X"),
			},
		},
	}
	expected := "Invalid first byte: 88"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readVersion1_ClosedEarly(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte("P"),
			},
		},
	}
	expected := "TCP session closed prior to PROXY line."
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readVersion1_TCPError(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte("P"),
			},
			readResponse{
				Error: fmt.Errorf("EXPECTED"),
			},
		},
	}
	expected := "TCP error reading PROXY line: EXPECTED"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readVersion1_NonProxyStart(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte("P"),
			},
			readResponse{
				Data: []byte("WTFWTFWTF"),
			},
		},
	}
	expected := "First line did not start with 'PROXY '"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readVersion1_LineTooLong(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: makeReplies([]byte(strings.Repeat("X", 200))),
	}
	expected := "PROXY line malformed (too long)"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readVersion1_ErrorReading(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte("P"),
			},
			readResponse{
				Data: []byte("ROXY "),
			},
			readResponse{
				Error: fmt.Errorf("EXPECTED"),
			},
		},
	}
	expected := "Error reading PROXY line: EXPECTED"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readVersion1_InvalidByteResponse(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte("P"),
			},
			readResponse{
				Data: []byte("ROXY "),
			},
			readResponse{
				Data: []byte("XX"),
			},
		},
	}
	expected := "Invalid number of bytes read from socket: 2"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readVersion1_WrongNumberOfElements(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: makeReplies([]byte("1 2 3 4 5 6 7 8 9\n\r")),
	}
	expected := "PROXY line is malformed, incorrect number of elements: 10"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readVersion1_InvalidProtocol(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: makeReplies([]byte("XXX 1 2 3 4\n\r")),
	}
	expected := "Protocol element of the PROXY line is invalid: XXX"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readVersion1_BadSourcePort(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: makeReplies(
			[]byte("TCP4 1.1.1.1 2.2.2.2 X 1\n\r")),
	}
	expected := `destination port element of the PROXY line is not an int: ` +
		`strconv.ParseInt: parsing "X": invalid syntax`
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readVersion1_SecondDeadlineError(t *testing.T) {
	conn := &ConnMock{
		DeadlineErrors: []error{nil, fmt.Errorf("EXPECTED")},
		ReadReplies: makeReplies(
			[]byte("TCP4 1.1.1.1 2.2.2.2 1 2\n\r")),
	}
	expected := "Error unsetting deadline on the socket: EXPECTED"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readVersion1_Success(t *testing.T) {
	errors := make([]error, 0, 2)
	pl := &ProxyListener{
		listener: &ListenerMock{
			AcceptReplies: []acceptResponse{
				acceptResponse{Conn: &ConnMock{
					ReadReplies: makeReplies(
						[]byte("TCP4 1.1.1.1 2.2.2.2 1 2\n\r"),
						[]byte("OK")),
				}},
			},
		},
		errorChan:  make(chan error),
		acceptChan: make(chan net.Conn),
		ProtocolError: func(err error) {
			errors = append(errors, err)
		},
		ProtocolDeadline: time.Second,
	}
	pl.waitGroup.Add(1)
	go pl.goAcceptRoutine()

	// Check the results.
	if len(errors) != 0 {
		t.Logf("%#v", errors)
		t.Fatalf("No error expected, at least one returned.")
	}
	select {
	case conn := <-pl.acceptChan:
		data := make([]byte, 2)
		if conn.RemoteAddr().String() != "1.1.1.1:1" {
			t.Fatalf(
				"The Addr was not parsed properly (expected 1.1.1.1:1): %s",
				conn.RemoteAddr())
		} else if n, err := conn.Read(data); err != nil {
			t.Fatalf("Error reading post-proxy data: %s", err)
		} else if n != 2 {
			t.Fatalf("Unknown read size: %d", n)
		} else if !bytes.Equal(data, []byte("OK")) {
			t.Fatalf("Unexpected response (expected OK): %#v", data)
		}
	case <-time.NewTimer(time.Millisecond * 10).C:
		t.Fatalf("No connection was passed to the channel.")
	}

	// Stop the processor.
	<-pl.errorChan
	pl.Stop()
}

func TestProxyListener_readBinary_EarlyHeaderEOF(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte{0x0D},
			},
		},
	}
	expected := "Error reading binary header data: EOF"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readBinary_TCPError(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte{0x0D},
			},
			readResponse{
				Error: fmt.Errorf("EXPECTED"),
			},
		},
	}
	expected := "Error reading binary header data: EXPECTED"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readBinary_InvalidHeader(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte{0x0D},
			},
			readResponse{
				Data: bytes.Repeat([]byte{0}, 15),
			},
		},
	}
	expected := "Static header bytes did not match expected value."
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readBinary_InvalidVersion(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte{0x0D},
			},
			readResponse{
				Data: []byte{
					0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
					0x51, 0x55, 0x49, 0x54, 0x10,
				},
			},
			readResponse{
				Data: []byte{0xF0, 0x00, 0x00, 0x00},
			},
		},
	}
	expected := "Version in the header was not 2: 15"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readBinary_InvalidCommandBits(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte{0x0D},
			},
			readResponse{
				Data: []byte{
					0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
					0x51, 0x55, 0x49, 0x54, 0x10,
				},
			},
			readResponse{
				Data: []byte{0x2F, 0x00, 0x00, 0x00},
			},
		},
	}
	expected := "Command bits contained an invalid value: 15"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readBinary_InvalidFamilyBits(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte{0x0D},
			},
			readResponse{
				Data: []byte{
					0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
					0x51, 0x55, 0x49, 0x54, 0x10,
				},
			},
			readResponse{
				Data: []byte{0x20, 0xF0, 0x00, 0x00},
			},
		},
	}
	expected := "Family bits contained an invalid value: 15"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readBinary_InvalidProtocolBits(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte{0x0D},
			},
			readResponse{
				Data: []byte{
					0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
					0x51, 0x55, 0x49, 0x54, 0x10,
				},
			},
			readResponse{
				Data: []byte{0x20, 0x0F, 0x00, 0x00},
			},
		},
	}
	expected := "Protocol bits contained an invalid value: 15"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readBinary_LengthTooLarge(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte{0x0D},
			},
			readResponse{
				Data: []byte{
					0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
					0x51, 0x55, 0x49, 0x54, 0x10,
				},
			},
			readResponse{
				Data: []byte{0x20, 0x00, 0xEE, 0xFF},
			},
		},
	}
	expected := "Length integer is too large to be valid: 61183"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readBinary_ErrReadingAddressData(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte{0x0D},
			},
			readResponse{
				Data: []byte{
					0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
					0x51, 0x55, 0x49, 0x54, 0x10,
				},
			},
			readResponse{
				Data: []byte{0x20, 0x00, 0x00, 0x01},
			},
			readResponse{
				Error: fmt.Errorf("EXPECTED"),
			},
		},
	}
	expected := "Error reading binary address data: EXPECTED"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readBinary_SecondDeadlineError(t *testing.T) {
	conn := &ConnMock{
		DeadlineErrors: []error{nil, fmt.Errorf("EXPECTED")},
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte{0x0D},
			},
			readResponse{
				Data: []byte{
					0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
					0x51, 0x55, 0x49, 0x54, 0x10,
				},
			},
			readResponse{
				Data: []byte{0x20, 0x00, 0x00, 0x00},
			},
		},
	}
	expected := "Error unsetting deadline on the socket: EXPECTED"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readBinary_LocalSuccess(t *testing.T) {
	errors := make([]error, 0, 2)
	pl := &ProxyListener{
		listener: &ListenerMock{
			AcceptReplies: []acceptResponse{
				acceptResponse{Conn: &ConnMock{
					RawRemoteAddr: &AddrMock{
						network: "net",
						str:     "1.1.1.1:1",
					},
					ReadReplies: []readResponse{
						readResponse{
							Data: []byte{0x0D},
						},
						readResponse{
							Data: []byte{
								0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
								0x51, 0x55, 0x49, 0x54, 0x10,
							},
						},
						readResponse{
							Data: []byte{0x20, 0x00, 0x00, 0x00},
						},
						readResponse{
							Data: []byte("OK"),
						},
					},
				}},
			},
		},
		errorChan:  make(chan error),
		acceptChan: make(chan net.Conn),
		ProtocolError: func(err error) {
			errors = append(errors, err)
		},
		ProtocolDeadline: time.Second,
	}
	pl.waitGroup.Add(1)
	go pl.goAcceptRoutine()

	// Check the results.
	if len(errors) != 0 {
		t.Logf("%#v", errors)
		t.Fatalf("No error expected, at least one returned.")
	}
	select {
	case conn := <-pl.acceptChan:
		data := make([]byte, 2)
		if conn.RemoteAddr().String() != "1.1.1.1:1" {
			t.Fatalf(
				"The Addr was not passed through properly: %s",
				conn.RemoteAddr())
		} else if n, err := conn.Read(data); err != nil {
			t.Fatalf("Error reading post-proxy data: %s", err)
		} else if n != 2 {
			t.Fatalf("Unknown read size: %d", n)
		} else if !bytes.Equal(data, []byte("OK")) {
			t.Fatalf("Unexpected response (expected OK): %#v", data)
		}
	case <-time.NewTimer(time.Millisecond * 10).C:
		t.Fatalf("No connection was passed to the channel.")
	}

	// Stop the processor.
	<-pl.errorChan
	pl.Stop()
}

func TestProxyListener_readBinary_TCPIPv4AddressTooLong(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte{0x0D},
			},
			readResponse{
				Data: []byte{
					0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
					0x51, 0x55, 0x49, 0x54, 0x10,
				},
			},
			readResponse{
				Data: []byte{0x21, 0x11, 0x00, 0x00},
			},
		},
	}
	expected := "Invalid address length for a INET proxy (should be 12): 0"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readBinary_TCPOverIPv4(t *testing.T) {
	errors := make([]error, 0, 2)
	pl := &ProxyListener{
		listener: &ListenerMock{
			AcceptReplies: []acceptResponse{
				acceptResponse{Conn: &ConnMock{
					RawRemoteAddr: &AddrMock{
						network: "net",
						str:     "0.0.0.0:0",
					},
					ReadReplies: []readResponse{
						readResponse{
							Data: []byte{0x0D},
						},
						readResponse{
							Data: []byte{
								0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
								0x51, 0x55, 0x49, 0x54, 0x10,
							},
						},
						readResponse{
							Data: []byte{0x21, 0x11, 0x00, 0x0C},
						},
						readResponse{
							Data: []byte{0x01, 0x02, 0x03, 0x04},
						},
						readResponse{
							Data: []byte{0x05, 0x06, 0x07, 0x08},
						},
						readResponse{
							Data: []byte{0x09, 0x0A},
						},
						readResponse{
							Data: []byte{0x0B, 0x0C},
						},
						readResponse{
							Data: []byte("OK"),
						},
					},
				}},
			},
		},
		errorChan:  make(chan error),
		acceptChan: make(chan net.Conn),
		ProtocolError: func(err error) {
			errors = append(errors, err)
		},
		ProtocolDeadline: time.Second,
	}
	pl.waitGroup.Add(1)
	go pl.goAcceptRoutine()

	// Check the results.
	if len(errors) != 0 {
		t.Logf("%#v", errors)
		t.Fatalf("No error expected, at least one returned.")
	}
	select {
	case conn := <-pl.acceptChan:
		data := make([]byte, 2)
		if conn.RemoteAddr().String() != "1.2.3.4:2314" {
			t.Fatalf(
				"The Addr was not passed through properly: %s",
				conn.RemoteAddr())
		} else if n, err := conn.Read(data); err != nil {
			t.Fatalf("Error reading post-proxy data: %s", err)
		} else if n != 2 {
			t.Fatalf("Unknown read size: %d", n)
		} else if !bytes.Equal(data, []byte("OK")) {
			t.Fatalf("Unexpected response (expected OK): %#v", data)
		}
	case <-time.NewTimer(time.Millisecond * 10).C:
		t.Fatalf("No connection was passed to the channel.")
	}

	// Stop the processor.
	<-pl.errorChan
	pl.Stop()
}

func TestProxyListener_readBinary_UDPIPv4AddressTooLong(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte{0x0D},
			},
			readResponse{
				Data: []byte{
					0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
					0x51, 0x55, 0x49, 0x54, 0x10,
				},
			},
			readResponse{
				Data: []byte{0x21, 0x12, 0x00, 0x00},
			},
		},
	}
	expected := "Invalid address length for a INET proxy (should be 12): 0"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readBinary_UDPOverIPv4(t *testing.T) {
	errors := make([]error, 0, 2)
	pl := &ProxyListener{
		listener: &ListenerMock{
			AcceptReplies: []acceptResponse{
				acceptResponse{Conn: &ConnMock{
					RawRemoteAddr: &AddrMock{
						network: "net",
						str:     "0.0.0.0:0",
					},
					ReadReplies: []readResponse{
						readResponse{
							Data: []byte{0x0D},
						},
						readResponse{
							Data: []byte{
								0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
								0x51, 0x55, 0x49, 0x54, 0x10,
							},
						},
						readResponse{
							Data: []byte{0x21, 0x12, 0x00, 0x0C},
						},
						readResponse{
							Data: []byte{0x01, 0x02, 0x03, 0x04},
						},
						readResponse{
							Data: []byte{0x05, 0x06, 0x07, 0x08},
						},
						readResponse{
							Data: []byte{0x09, 0x0A},
						},
						readResponse{
							Data: []byte{0x0B, 0x0C},
						},
						readResponse{
							Data: []byte("OK"),
						},
					},
				}},
			},
		},
		errorChan:  make(chan error),
		acceptChan: make(chan net.Conn),
		ProtocolError: func(err error) {
			errors = append(errors, err)
		},
		ProtocolDeadline: time.Second,
	}
	pl.waitGroup.Add(1)
	go pl.goAcceptRoutine()

	// Check the results.
	if len(errors) != 0 {
		t.Logf("%#v", errors)
		t.Fatalf("No error expected, at least one returned.")
	}
	select {
	case conn := <-pl.acceptChan:
		data := make([]byte, 2)
		if conn.RemoteAddr().String() != "1.2.3.4:2314" {
			t.Fatalf(
				"The Addr was not passed through properly: %s",
				conn.RemoteAddr())
		} else if n, err := conn.Read(data); err != nil {
			t.Fatalf("Error reading post-proxy data: %s", err)
		} else if n != 2 {
			t.Fatalf("Unknown read size: %d", n)
		} else if !bytes.Equal(data, []byte("OK")) {
			t.Fatalf("Unexpected response (expected OK): %#v", data)
		}
	case <-time.NewTimer(time.Millisecond * 10).C:
		t.Fatalf("No connection was passed to the channel.")
	}

	// Stop the processor.
	<-pl.errorChan
	pl.Stop()
}

func TestProxyListener_readBinary_TCPIPv6AddressTooLong(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte{0x0D},
			},
			readResponse{
				Data: []byte{
					0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
					0x51, 0x55, 0x49, 0x54, 0x10,
				},
			},
			readResponse{
				Data: []byte{0x21, 0x21, 0x00, 0x00},
			},
		},
	}
	expected := "Invalid address length for a INET6 proxy (should be 36): 0"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readBinary_TCPOverIPv6(t *testing.T) {
	errors := make([]error, 0, 2)
	pl := &ProxyListener{
		listener: &ListenerMock{
			AcceptReplies: []acceptResponse{
				acceptResponse{Conn: &ConnMock{
					RawRemoteAddr: &AddrMock{
						network: "net",
						str:     "0.0.0.0:0",
					},
					ReadReplies: []readResponse{
						readResponse{
							Data: []byte{0x0D},
						},
						readResponse{
							Data: []byte{
								0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
								0x51, 0x55, 0x49, 0x54, 0x10,
							},
						},
						readResponse{
							Data: []byte{0x21, 0x21, 0x00, 0x24},
						},
						readResponse{
							Data: []byte{
								0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
								0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F,
							},
						},
						readResponse{
							Data: []byte{
								0x0F, 0x0E, 0x0D, 0x0C, 0x0B, 0x0A, 0x09, 0x08,
								0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01, 0x00,
							},
						},
						readResponse{
							Data: []byte{0x09, 0x0A},
						},
						readResponse{
							Data: []byte{0x0B, 0x0C},
						},
						readResponse{
							Data: []byte("OK"),
						},
					},
				}},
			},
		},
		errorChan:  make(chan error),
		acceptChan: make(chan net.Conn),
		ProtocolError: func(err error) {
			errors = append(errors, err)
		},
		ProtocolDeadline: time.Second,
	}
	pl.waitGroup.Add(1)
	go pl.goAcceptRoutine()

	// Check the results.
	if len(errors) != 0 {
		t.Logf("%#v", errors)
		t.Fatalf("No error expected, at least one returned.")
	}
	select {
	case conn := <-pl.acceptChan:
		addrExpected := "[1:203:405:607:809:a0b:c0d:e0f]:2314"
		data := make([]byte, 2)
		if conn.RemoteAddr().String() != addrExpected {
			t.Fatalf(
				"The Addr was not passed through properly: %s",
				conn.RemoteAddr())
		} else if n, err := conn.Read(data); err != nil {
			t.Fatalf("Error reading post-proxy data: %s", err)
		} else if n != 2 {
			t.Fatalf("Unknown read size: %d", n)
		} else if !bytes.Equal(data, []byte("OK")) {
			t.Fatalf("Unexpected response (expected OK): %#v", data)
		}
	case <-time.NewTimer(time.Millisecond * 10).C:
		t.Fatalf("No connection was passed to the channel.")
	}

	// Stop the processor.
	<-pl.errorChan
	pl.Stop()
}

func TestProxyListener_readBinary_UDPIPv6AddressTooLong(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte{0x0D},
			},
			readResponse{
				Data: []byte{
					0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
					0x51, 0x55, 0x49, 0x54, 0x10,
				},
			},
			readResponse{
				Data: []byte{0x21, 0x22, 0x00, 0x00},
			},
		},
	}
	expected := "Invalid address length for a INET6 proxy (should be 36): 0"
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_readBinary_UDPOverIPv6(t *testing.T) {
	errors := make([]error, 0, 2)
	pl := &ProxyListener{
		listener: &ListenerMock{
			AcceptReplies: []acceptResponse{
				acceptResponse{Conn: &ConnMock{
					RawRemoteAddr: &AddrMock{
						network: "net",
						str:     "0.0.0.0:0",
					},
					ReadReplies: []readResponse{
						readResponse{
							Data: []byte{0x0D},
						},
						readResponse{
							Data: []byte{
								0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
								0x51, 0x55, 0x49, 0x54, 0x10,
							},
						},
						readResponse{
							Data: []byte{0x21, 0x22, 0x00, 0x24},
						},
						readResponse{
							Data: []byte{
								0x0F, 0x0E, 0x0D, 0x0C, 0x0B, 0x0A, 0x09, 0x08,
								0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01, 0x00,
							},
						},
						readResponse{
							Data: []byte{
								0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
								0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F,
							},
						},
						readResponse{
							Data: []byte{0x09, 0x0A},
						},
						readResponse{
							Data: []byte{0x0B, 0x0C},
						},
						readResponse{
							Data: []byte("OK"),
						},
					},
				}},
			},
		},
		errorChan:  make(chan error),
		acceptChan: make(chan net.Conn),
		ProtocolError: func(err error) {
			errors = append(errors, err)
		},
		ProtocolDeadline: time.Second,
	}
	pl.waitGroup.Add(1)
	go pl.goAcceptRoutine()

	// Check the results.
	if len(errors) != 0 {
		t.Logf("%#v", errors)
		t.Fatalf("No error expected, at least one returned.")
	}
	select {
	case conn := <-pl.acceptChan:
		addrExpected := "[f0e:d0c:b0a:908:706:504:302:100]:2314"
		data := make([]byte, 2)
		if conn.RemoteAddr().String() != addrExpected {
			t.Fatalf(
				"The Addr was not passed through properly: %s",
				conn.RemoteAddr())
		} else if n, err := conn.Read(data); err != nil {
			t.Fatalf("Error reading post-proxy data: %s", err)
		} else if n != 2 {
			t.Fatalf("Unknown read size: %d", n)
		} else if !bytes.Equal(data, []byte("OK")) {
			t.Fatalf("Unexpected response (expected OK): %#v", data)
		}
	case <-time.NewTimer(time.Millisecond * 10).C:
		t.Fatalf("No connection was passed to the channel.")
	}

	// Stop the processor.
	<-pl.errorChan
	pl.Stop()
}

func TestProxyListener_readBinary_UnspportedCombination(t *testing.T) {
	conn := &ConnMock{
		ReadReplies: []readResponse{
			readResponse{
				Data: []byte{0x0D},
			},
			readResponse{
				Data: []byte{
					0x0AD, 0x0D, 0x0A, 0x00, 0x0D, 0x0A,
					0x51, 0x55, 0x49, 0x54, 0x10,
				},
			},
			readResponse{
				Data: []byte{0x21, 0x32, 0x00, 0x00},
			},
			readResponse{
				Data: bytes.Repeat([]byte{0}, 36),
			},
		},
	}
	expected := "Unsupported family/protocol combination."
	connectionTestWrapper(t, conn, expected)
}

func TestProxyListener_Accept_Error(t *testing.T) {
	p := &ProxyListener{
		errorChan:  make(chan error, 1),
		acceptChan: make(chan net.Conn, 1),
	}
	p.errorChan <- fmt.Errorf("EXPECTED")
	conn, err := p.Accept()
	if err == nil || err.Error() != "EXPECTED" {
		t.Fatalf("Expected error was not returned.")
	} else if conn != nil {
		t.Fatalf("Conn was not nil.")
	}
}

func TestProxyListener_Accept_Connection(t *testing.T) {
	p := &ProxyListener{
		errorChan:  make(chan error, 1),
		acceptChan: make(chan net.Conn, 1),
	}
	p.acceptChan <- &ConnMock{}
	conn, err := p.Accept()
	if err != nil {
		t.Fatalf("Unexpected error returned: %s", err)
	} else if conn == nil {
		t.Fatalf("Conn was nil and shouldn't have been.")
	}
}

func TestProxyListener_Addr(t *testing.T) {
	lm := &ListenerMock{
		RawAddr: &AddrMock{
			network: "xxx",
			str:     "yyy",
		},
	}
	p := &ProxyListener{
		listener: lm,
	}
	addr := p.Addr()
	if addr != lm.RawAddr {
		t.Fatalf("Addr() wasn't passed through properly.")
	}
}

func TestProxyListener_Close(t *testing.T) {
	lm := &ListenerMock{}
	p := &ProxyListener{
		listener: lm,
	}
	p.Close()
	if !lm.Closed {
		t.Fatalf("Listener was not closed.")
	}
}
