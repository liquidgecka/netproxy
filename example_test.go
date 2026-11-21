package netproxy_test

import (
	"fmt"
	"net/http"
	"time"

	"github.com/liquidgecka/netproxy/v2"
)

// Example demonstrates basic usage of netproxy in place of a normal net.Listener.
func Example() {
	listener, err := netproxy.Listen("tcp", ":0")
	if err != nil {
		fmt.Printf("Error: %s\n", err)
		return
	}
	defer func() { _ = listener.Close() }()

	// listener implements the net.Listener interface so Accept will return
	// in exactly the same way. Post Accept() the next byte read will be the
	// first byte sent by the client after the PROXY header.
	conn, err := listener.Accept()
	if err != nil {
		fmt.Printf("Error: %s\n", err)
		return
	}

	// RemoteAddr() will be set to point at the address given by the calling
	// proxy, rather than the real source IP.
	fmt.Printf("The proxied IP: %s\n", conn.RemoteAddr())
	_ = conn.Close()

	// The same listener can be used with a net/http.Server like so:
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := server.Serve(listener); err != nil {
		fmt.Printf("Server error: %s\n", err)
	}
}
