package netproxy

import (
	"fmt"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A simple helper that takes a byte array and makes it into a readReplies
// array for the ListenerMock.
func makeReplies(b []byte) []readResponse {
	rr := make([]readResponse, len(b)+1)
	rr[0] = readResponse{
		Data: []byte("PROXY "),
	}
	for i := range b {
		rr[i+1] = readResponse{
			Data: []byte{b[i]},
		}
	}
	return rr
}

func TestListenError(t *testing.T) {
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

func TestListenStartAndStop(t *testing.T) {
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
	pl.protocolErrorLog(fmt.Errorf("test1"))
	pl.ProtocolError = nil
	pl.protocolErrorLog(fmt.Errorf("test2"))

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

func TestProxyListener_goReadProxyRoutine_DeadlineError(t *testing.T) {
	errors := make([]error, 0, 2)
	pl := &ProxyListener{
		listener: &ListenerMock{
			AcceptReplies: []acceptResponse{
				acceptResponse{Conn: &ConnMock{
					DeadlineError: fmt.Errorf("EXPECTED"),
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
	<-pl.errorChan
	pl.Stop()

	// Make sure the error was received.
	expected := "Error setting deadline on the socket: EXPECTED"
	if len(errors) != 1 {
		t.Logf("%#v", errors)
		t.Fatalf("Expected exactly one error.")
	} else if errors[0].Error() != expected {
		t.Fatalf("Unknown error returned: %s", errors[0])
	}
}

func TestProxyListener_goReadProxyRoutine_ClosedEarly(t *testing.T) {
	errors := make([]error, 0, 2)
	pl := &ProxyListener{
		listener: &ListenerMock{
			AcceptReplies: []acceptResponse{
				acceptResponse{Conn: &ConnMock{
					ReadReplies: []readResponse{
						readResponse{Data: []byte("")},
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
	<-pl.errorChan
	pl.Stop()

	// Make sure the error was received.
	expected := "TCP session closed prior to PROXY line."
	if len(errors) != 1 {
		t.Logf("%#v", errors)
		t.Fatalf("Expected exactly one error.")
	} else if errors[0].Error() != expected {
		t.Fatalf("Unknown error returned: %s", errors[0])
	}
}

func TestProxyListener_goReadProxyRoutine_TCPError(t *testing.T) {
	errors := make([]error, 0, 2)
	pl := &ProxyListener{
		listener: &ListenerMock{
			AcceptReplies: []acceptResponse{
				acceptResponse{Conn: &ConnMock{
					ReadReplies: []readResponse{
						readResponse{Error: fmt.Errorf("EXPECTED")},
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
	<-pl.errorChan
	pl.Stop()

	// Make sure the error was received.
	expected := "TCP error reading from connection: EXPECTED"
	if len(errors) != 1 {
		t.Logf("%#v", errors)
		t.Fatalf("Expected exactly one error.")
	} else if errors[0].Error() != expected {
		t.Fatalf("Unknown error returned: %s", errors[0])
	}
}

func TestProxyListener_goReadProxyRoutine_LineTooLong(t *testing.T) {
	errors := make([]error, 0, 2)
	pl := &ProxyListener{
		listener: &ListenerMock{
			AcceptReplies: []acceptResponse{
				acceptResponse{Conn: &ConnMock{
					ReadReplies: makeReplies([]byte(strings.Repeat("X", 100))),
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
	<-pl.errorChan
	pl.Stop()

	// Make sure the error was received.
	expected := "PROXY line malformed (too long)"
	if len(errors) != 1 {
		t.Logf("%#v", errors)
		t.Fatalf("Expected exactly one error.")
	} else if errors[0].Error() != expected {
		t.Fatalf("Unknown error returned: %s", errors[0])
	}
}

func TestProxyListener_goReadProxyRoutine_ErrorReading(t *testing.T) {
	errors := make([]error, 0, 2)
	pl := &ProxyListener{
		listener: &ListenerMock{
			AcceptReplies: []acceptResponse{
				acceptResponse{Conn: &ConnMock{
					ReadReplies: []readResponse{
						readResponse{
							Data: []byte("PROXY "),
						},
						readResponse{
							Error: fmt.Errorf("EXPECTED"),
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
	<-pl.errorChan
	pl.Stop()

	// Make sure the error was received.
	expected := "Error reading PROXY line: EXPECTED"
	if len(errors) != 1 {
		t.Logf("%#v", errors)
		t.Fatalf("Expected exactly one error.")
	} else if errors[0].Error() != expected {
		t.Fatalf("Unknown error returned: %s", errors[0])
	}
}

func TestProxyListener_goReadProxyRoutine_InvalidByteResponse(t *testing.T) {
	errors := make([]error, 0, 2)
	pl := &ProxyListener{
		listener: &ListenerMock{
			AcceptReplies: []acceptResponse{
				acceptResponse{Conn: &ConnMock{
					ReadReplies: []readResponse{
						readResponse{
							Data: []byte("PROXY "),
						},
						readResponse{
							Data: []byte("XX"),
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
	<-pl.errorChan
	pl.Stop()

	// Make sure the error was received.
	expected := "Invalid number of bytes read from socket: 2"
	if len(errors) != 1 {
		t.Logf("%#v", errors)
		t.Fatalf("Expected exactly one error.")
	} else if errors[0].Error() != expected {
		t.Fatalf("Unknown error returned: %s", errors[0])
	}
}

func TestProxyListener_goReadProxyRoutine_WrongNumberOfElements(t *testing.T) {
	errors := make([]error, 0, 2)
	pl := &ProxyListener{
		listener: &ListenerMock{
			AcceptReplies: []acceptResponse{
				acceptResponse{Conn: &ConnMock{
					ReadReplies: makeReplies([]byte("1 2 3 4 5 6 7 8 9\n\r")),
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
	<-pl.errorChan
	pl.Stop()

	// Make sure the error was received.
	expected := "PROXY line is malformed, incorrect number of elements: 10"
	if len(errors) != 1 {
		t.Logf("%#v", errors)
		t.Fatalf("Expected exactly one error.")
	} else if errors[0].Error() != expected {
		t.Fatalf("Unknown error returned: %s", errors[0])
	}
}

func TestProxyListener_goReadProxyRoutine_BadSourcePort(t *testing.T) {
	errors := make([]error, 0, 2)
	pl := &ProxyListener{
		listener: &ListenerMock{
			AcceptReplies: []acceptResponse{
				acceptResponse{Conn: &ConnMock{
					ReadReplies: makeReplies(
						[]byte("TCP4 1.1.1.1 2.2.2.2 X 1\n\r")),
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
	<-pl.errorChan
	pl.Stop()

	// Make sure the error was received.
	expected := `destination port element of the PROXY line is not an int: ` +
		`strconv.ParseInt: parsing "X": invalid syntax`
	if len(errors) != 1 {
		t.Logf("%#v", errors)
		t.Fatalf("Expected exactly one error.")
	} else if errors[0].Error() != expected {
		t.Fatalf("Unknown error returned: %s", errors[0])
	}
}

func TestProxyListener_goReadProxyRoutine_Success(t *testing.T) {
	errors := make([]error, 0, 2)
	pl := &ProxyListener{
		listener: &ListenerMock{
			AcceptReplies: []acceptResponse{
				acceptResponse{Conn: &ConnMock{
					ReadReplies: makeReplies(
						[]byte("TCP4 1.1.1.1 2.2.2.2 1 2\n\r")),
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
		if conn.RemoteAddr().String() != "1.1.1.1:1" {
			t.Fatalf(
				"The Addr was not parsed properly (expected 1.1.1.1:1): %s",
				conn.RemoteAddr())
		}
	case <-time.NewTimer(time.Millisecond * 10).C:
		t.Fatalf("No connection was passed to the channel.")
	}

	// Stop the processor.
	<-pl.errorChan
	pl.Stop()
}
