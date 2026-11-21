package main

import (
	"io"
)

// relayStdio copies frames verbatim between client and upstream MCP server.
// No parsing, no rewriting — id/protocolVersion/progressToken survive by construction.
// Blocks until either side closes; half-close the upstream write side so the
// server sees EOF and can flush its reply before we drain stdout.
func relayStdio(clientIn io.Reader, clientOut io.Writer, upstreamIn io.Writer, upstreamOut io.Reader) {
	go func() {
		io.Copy(upstreamIn, clientIn) //nolint:errcheck // passthrough: upstream close ends the session
		if c, ok := upstreamIn.(interface{ Close() error }); ok {
			c.Close()
		}
	}()
	io.Copy(clientOut, upstreamOut) //nolint:errcheck // passthrough: server close ends the session
}
