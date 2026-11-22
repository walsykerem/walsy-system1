package main

import (
	"io"
	"net"
	"os"
)

// listenUnix binds a unix socket with 0600 permissions. It refuses symlinks
// and live sockets, and removes only a genuinely stale socket file.
func listenUnix(path string) (net.Listener, error) {
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, &os.PathError{Op: "listen", Path: path, Err: os.ErrExist}
		}
		// A live listener answers a dial; only a stale socket is removable.
		if c, err := net.DialTimeout("unix", path, 100_000_000); err == nil {
			c.Close()
			return nil, &os.PathError{Op: "listen", Path: path, Err: os.ErrExist}
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
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
				_, _ = io.Copy(io.Discard, c) // no upstream: transparent no-op
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

// relayConn bridges two byte streams verbatim with half-close on each side:
// when one side ends, only its write half is closed, so the peer can flush.
func relayConn(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(b, a)
		halfCloseWriter(b)
		done <- struct{}{}
	}()
	_, _ = io.Copy(a, b)
	halfCloseWriter(a)
	<-done
}
