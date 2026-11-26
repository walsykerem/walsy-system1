package app

import (
	"bufio"
	"io"
	"net"
	"os"
	"time"
)

// listenUnix binds a unix socket with 0600 permissions. It refuses symlinks
// and live sockets, and removes only a genuinely stale socket file.
func listenUnix(path string) (net.Listener, error) {
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, &os.PathError{Op: "listen", Path: path, Err: os.ErrExist}
		}
		if st.Mode()&os.ModeSocket == 0 {
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

// unixMaxConns caps concurrent served connections, so a burst of accepted
// sockets cannot spawn goroutines without bound. Beyond the cap the accept
// loop waits; new clients queue in the listen backlog.
const unixMaxConns = 64

// serveUnix accepts clients until the listener closes, at most unixMaxConns at
// once. dial supplies one upstream connection per client; nil is a transparent
// no-op (drain + EOF), never a 503 or a synthetic error frame.
func serveUnix(l net.Listener, dial func() (net.Conn, error)) {
	sem := make(chan struct{}, unixMaxConns)
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		// Bound a silent client so a stalled frame cannot pin a slot forever.
		// Same 30s budget as the HTTP path; each connection gets its own clock.
		// Use the net.Conn interface so test/listed wrappers are covered too.
		_ = c.SetDeadline(time.Now().Add(httpReadTimeout))
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			defer c.Close()
			if dial == nil {
				// No upstream stays a transparent no-op, but every frame still
				// passes the shared decision hook before it is discarded.
				d := newDecisionBridge()
				br := bufio.NewReader(c)
				for {
					raw, err := readFrame(br)
					if err != nil {
						if err == io.EOF && len(raw) > 0 {
							_ = d.decideFrame(raw)
						}
						return
					}
					_ = d.decideFrame(raw)
				}
			}
			up, err := dial()
			if err != nil {
				// On unix sockets, half-close then drain so an in-flight client
				// Write cannot hit a closed socket: transparent EOF, no broken
				// pipe. Other conns keep the immediate passthrough close so a
				// dial failure frees the concurrency slot at once.
				if uc, ok := c.(*net.UnixConn); ok {
					_ = uc.CloseWrite()
					_, _ = io.Copy(io.Discard, c)
				}
				return
			}
			bridgeStdio(c, c, up, up)
		}()
	}
}
