package netproxy

import (
	"bytes"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

type handler struct{}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
	io.Copy(ioutil.Discard, r.Body)
}

// Benchmarks the default server so we have a baseline of performance
// without the PROXY handler.
func BenchmarkNetServe(b *testing.B) {
	server := &http.Server{
		Handler:        new(handler),
		ReadTimeout:    time.Second,
		WriteTimeout:   time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	listener, err := net.Listen("tcp", ":")
	if err != nil {
		b.Fatalf("%s", err)
	}
	serverWG := sync.WaitGroup{}
	serverWG.Add(1)
	go func() {
		serverWG.Done()
		server.Serve(listener)
	}()
	defer serverWG.Wait()
	defer server.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn, err := net.DialTimeout(
			"tcp", listener.Addr().String(), time.Second)
		if err != nil {
			b.Fatalf("%s", err)
			return
		}
		buffer := bytes.NewBuffer([]byte("" +
			"GET / HTTP/1.1\r\n" +
			"Host: example.com\r\n" +
			"User-Agent: golang-test\r\n" +
			"Accept: */*\r\n" +
			"\r\n"))
		if _, err := io.Copy(conn, buffer); err != nil {
			b.Fatalf("%s", err)
			return
		}
		conn.(*net.TCPConn).CloseWrite()
		if _, err := io.Copy(ioutil.Discard, conn); err != nil {
			b.Fatalf("%s", err)
			return
		}
		conn.Close()
	}
	b.StopTimer()
}

// Benchmarks the PROXY implementation.
func BenchmarkProxyNetServe(b *testing.B) {
	server := &http.Server{
		Handler:        new(handler),
		ReadTimeout:    time.Second,
		WriteTimeout:   time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	listener, err := Listen("tcp", ":")
	if err != nil {
		b.Fatalf("%s", err)
	}
	serverWG := sync.WaitGroup{}
	serverWG.Add(1)
	go func() {
		serverWG.Done()
		server.Serve(listener)
	}()
	defer serverWG.Wait()
	defer server.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn, err := net.DialTimeout(
			"tcp", listener.Addr().String(), time.Second)
		if err != nil {
			b.Fatalf("%s", err)
			return
		}
		buffer := bytes.NewBuffer([]byte("" +
			"PROXY tcp4 1.2.3.4 5.6.7.8 1 2\r\n" +
			"GET / HTTP/1.1\r\n" +
			"Host: example.com\r\n" +
			"User-Agent: golang-test\r\n" +
			"Accept: */*\r\n" +
			"\r\n"))
		if _, err := io.Copy(conn, buffer); err != nil {
			b.Fatalf("%s", err)
			return
		}
		conn.(*net.TCPConn).CloseWrite()
		if _, err := io.Copy(ioutil.Discard, conn); err != nil {
			b.Fatalf("%s", err)
			return
		}
		conn.Close()
	}
	b.StopTimer()
}

// Benchmarks the default server so we have a baseline of performance
// without the PROXY handler.
func BenchmarkConnListen(b *testing.B) {
	listener, err := net.Listen("tcp", ":")
	if err != nil {
		b.Fatalf("%s", err)
	}
	serverWG := sync.WaitGroup{}
	serverWG.Add(1)
	go func() {
		defer serverWG.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			serverWG.Add(1)
			go func(conn net.Conn) {
				conn.Write([]byte("OK"))
				conn.Close()
				serverWG.Done()
			}(conn)
		}
	}()
	defer serverWG.Wait()
	defer listener.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn, err := net.DialTimeout(
			"tcp", listener.Addr().String(), time.Second)
		if err != nil {
			b.Fatalf("%s", err)
			return
		}
		buffer := bytes.NewBuffer([]byte{})
		if _, err := io.Copy(conn, buffer); err != nil {
			b.Fatalf("%s", err)
			return
		}
		data := make([]byte, 100)
		if n, err := conn.Read(data); err != nil {
			b.Fatalf("%s", err)
			return
		} else if n != 2 {
			b.Fatalf("Invalid number of bytes read: %d", n)
		} else if bytes.Equal(data, []byte("OK")) {
			b.Fatalf("Connection failed to receive valid data.")
		}
		conn.Close()
	}
	b.StopTimer()
}

// Benchmarks a simple connect with the PROXY setup.
func BenchmarkProxyConnListen(b *testing.B) {
	listener, err := Listen("tcp", ":")
	if err != nil {
		b.Fatalf("%s", err)
	}
	serverWG := sync.WaitGroup{}
	serverWG.Add(1)
	go func() {
		defer serverWG.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			serverWG.Add(1)
			go func(conn net.Conn) {
				conn.Write([]byte("OK"))
				conn.Close()
				serverWG.Done()
			}(conn)
		}
	}()
	defer serverWG.Wait()
	defer listener.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn, err := net.DialTimeout(
			"tcp", listener.Addr().String(), time.Second)
		if err != nil {
			b.Fatalf("%s", err)
			return
		}
		buffer := bytes.NewBuffer(
			[]byte("PROXY TCP4 1.2.3.4 5.6.7.8 100 200\r\n"))
		if _, err := io.Copy(conn, buffer); err != nil {
			b.Fatalf("%s", err)
			return
		}
		data := make([]byte, 100)
		if n, err := conn.Read(data); err != nil {
			b.Fatalf("%s", err)
			return
		} else if n != 2 {
			b.Fatalf("Invalid number of bytes read: %d", n)
		} else if bytes.Equal(data, []byte("OK")) {
			b.Fatalf("Connection failed to receive valid data.")
		}
		conn.Close()
	}
	b.StopTimer()
}
