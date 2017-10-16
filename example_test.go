package netproxy_test

import (
	"fmt"
	"net/http"

	"github.com/liquidgecka/netproxy"
)

// Basic example showing how to use this package in place of a normal
// net.Listener.
func Example() {
	listener, err := netproxy.Listen("tcp", ":")
	if err != nil {
		fmt.Printf("Error: %s\n", err)
		return
	}

	// listener implements the net.Listener interface so Accept will return
	// in exactly the same way. Post Accept() the next byte read will be the
	// first byte send to the proxy.
	conn, err := listener.Accept()
	if err != nil {
		fmt.Printf("Error: %s\n", err)
		return
	}

	// RemoteAddr() will be set to point at the address given by the calling
	// proxy, rather than the real source IP.
	fmt.Printf("The proxied IP: %s", conn.RemoteAddr())

	// The same listener can be used with an net/http.Server{} like so:
	server := &http.Server{}
	server.Serve(listener)
}
