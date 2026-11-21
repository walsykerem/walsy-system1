package main

import (
	"io"
	"net"
)

// listenUnix binds a unix socket. Go unlinks the socket file on Close;
// no discovery file, no port, no retry.
func listenUnix(path string) (net.Listener, error) {
	return net.Listen("unix", path)
}

// serveUnix accepts clients until the listener closes. dial supplies one
// upstream connection per client; nil is a transparent no-op (drain + EOF),
// never a 503 or a synthetic error frame.
func serveUnix(l net.Listener, dial func() (net.Conn, error)) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			if dial == nil {
				io.Copy(io.Discard, c) //nolint:errcheck // no upstream: transparent no-op
				return
			}
			up, err := dial()
			if err != nil {
				return // passthrough: client just sees EOF
			}
			relayConn(c, up)
		}()
	}
}

// relayConn bridges two byte streams verbatim. First side to end closes both,
// unblocking the other copy; no frame parsing, no rewriting.
func relayConn(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 1)
	go func() {
		io.Copy(b, a) //nolint:errcheck // passthrough: close ends the session
		b.Close()
		done <- struct{}{}
	}()
	io.Copy(a, b) //nolint:errcheck // passthrough: close ends the session
	a.Close()
	<-done
}
