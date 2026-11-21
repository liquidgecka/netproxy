# netproxy

[![CI](https://github.com/liquidgecka/netproxy/actions/workflows/ci.yml/badge.svg)](https://github.com/liquidgecka/netproxy/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/liquidgecka/netproxy.svg)](https://pkg.go.dev/github.com/liquidgecka/netproxy)

## Overview

`netproxy` is a Go package that wraps `net.Listener` to accept HAProxy and Google Cloud TCP load balancer `PROXY` protocol headers (versions 1 and 2) and expose the original client address through `net.Conn.RemoteAddr()`.

## Installation

```sh
go get github.com/liquidgecka/netproxy
```

Requires Go 1.26 or later.

## Usage

```go
listener, err := netproxy.Listen("tcp", ":8080")
if err != nil {
	log.Fatal(err)
}
defer listener.Close()

server := &http.Server{}
log.Fatal(server.Serve(listener))
```

To wrap an existing `net.Listener` with a header read deadline and error hook:

```go
pl := netproxy.ListenWrapper(
	rawListener,
	netproxy.WithProtocolDeadline(5*time.Second),
	netproxy.WithProtocolError(func(err error) {
		log.Printf("proxy protocol error: %v", err)
	}),
)
defer pl.Stop()
```

## License

See [LICENSE](LICENSE) for details.
