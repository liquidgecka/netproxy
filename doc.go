// Package netproxy implements a [net.Listener] wrapper for the HAProxy PROXY
// protocol (versions 1 and 2), allowing servers behind TCP load balancers such
// as HAProxy or Google Cloud TCP Proxy load balancers to observe the original
// client address transparently via [net.Conn.RemoteAddr].
//
// Details of the protocol can be found in the HAProxy specification:
// https://www.haproxy.org/download/1.8/doc/proxy-protocol.txt
package netproxy
