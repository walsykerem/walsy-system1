package app

import "io"

// relayStdio copies frames verbatim between client and upstream, running the
// decision bridge on the real JSON-RPC stream. Any SO failure/absence is a
// passthrough; the upstream write side is half-closed so replies can flush.
func relayStdio(clientIn io.Reader, clientOut io.Writer, upstreamIn io.Writer, upstreamOut io.Reader) {
	bridgeStdio(clientIn, clientOut, upstreamIn, upstreamOut)
}
