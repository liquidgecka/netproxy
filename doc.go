// A simply library that implements the HAProxy 'PROXY' protocol in a way
// that makes it as seamless as possible for users. This basically wraps
// around net.Listener in a way that makes it possible to drop into
// and existing setup without any changes to the HttpServer or other
// RPC listeners. It modifies the RemoteAddr on the connection to match
// the values given during connection initiation.
//
// Details of the protocl can be found here:
// https://www.haproxy.org/download/1.8/doc/proxy-protocol.txt
package netproxy
