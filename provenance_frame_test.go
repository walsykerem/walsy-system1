package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// Frame-level equivalence: the four transports share decodeFrame, so one
// representative frame per surface must survive it byte-for-byte.
func TestFA15FrameLevelEquivalence(t *testing.T) {
	frames := [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`),
		[]byte(`{"jsonrpc":"2.0","id":"u-1","method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`),
		[]byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"tok"}}`),
		[]byte(`{"jsonrpc":"2.0","id":9,"result":{"content":[{"type":"text","text":"ok"}]}}`),
	}
	for i, raw := range frames {
		f, err := decodeFrame(raw)
		if err != nil {
			t.Fatalf("frame %d decode: %v", i, err)
		}
		if !bytes.Equal(f.Raw, raw) {
			t.Fatalf("frame %d changed: got %s want %s", i, f.Raw, raw)
		}
	}
}

// The embedded decision items are the same bytes hashed into /health.
func TestFA15ItemsDigestMatchesEmbedded(t *testing.T) {
	h := healthFingerprint()
	sum := sha256.Sum256(decisionItemsJSON)
	if got := h["items_sha256"].(string); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("items digest %s want %s", got, hex.EncodeToString(sum[:]))
	}
}
