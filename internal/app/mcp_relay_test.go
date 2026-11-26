package app

import (
	"bytes"
	"encoding/json"
	"testing"
)

func mustFrame(t *testing.T, raw []byte) frame {
	t.Helper()
	f, err := decodeFrame(raw)
	if err != nil {
		t.Fatalf("decodeFrame: %v", err)
	}
	return f
}

func TestAddRouteCapabilityNoExtension(t *testing.T) {
	result := []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{"listChanged":true},"logging":{}},"serverInfo":{"name":"x"}}}`)
	f := mustFrame(t, result)
	if !isInitializeResult(f) {
		t.Fatal("isInitializeResult = false")
	}
	out := addRouteCapability(f.Result)
	if bytes.Equal(out, f.Result) {
		t.Fatal("marker not added")
	}
	var r struct {
		Protocol     json.RawMessage            `json:"protocolVersion"`
		Capabilities map[string]json.RawMessage `json:"capabilities"`
		ServerInfo   map[string]any             `json:"serverInfo"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("invalid output JSON: %v\n%s", err, out)
	}
	if string(r.Protocol) != `"2025-06-18"` {
		t.Fatalf("protocolVersion changed: %s", r.Protocol)
	}
	if r.ServerInfo["name"] != "x" {
		t.Fatalf("serverInfo changed: %v", r.ServerInfo)
	}
	for _, k := range []string{"tools", "logging"} {
		if _, ok := r.Capabilities[k]; !ok {
			t.Fatalf("capability %q lost", k)
		}
	}
	exp, ok := r.Capabilities["experimental"]
	if !ok {
		t.Fatal("experimental missing")
	}
	var m map[string]struct {
		Enabled bool   `json:"enabled"`
		Mode    string `json:"mode"`
	}
	if err := json.Unmarshal(exp, &m); err != nil {
		t.Fatal(err)
	}
	marker, ok := m[markerKey]
	if !ok {
		t.Fatalf("marker missing from %s", exp)
	}
	if marker.Enabled || marker.Mode != "relay" {
		t.Fatalf("marker = %+v, want default-off relay", marker)
	}
	// marker added in place: protocolVersion and unknown fields keep their original bytes
	if !bytes.Contains(out, []byte(`"protocolVersion":"2025-06-18"`)) {
		t.Fatalf("protocolVersion bytes changed: %s", out)
	}
	if !bytes.Contains(out, []byte(`"logging":{}`)) {
		t.Fatalf("unknown capability bytes changed: %s", out)
	}
}

// malformed JSON -> decodeFrame error, addRouteCapability returns input unchanged.
func TestMalformedPassthrough(t *testing.T) {
	bad := []byte(`{"id":1,"method":`)
	if _, err := decodeFrame(bad); err == nil {
		t.Fatal("decodeFrame accepted malformed JSON")
	}
	if got := addRouteCapability(bad); !bytes.Equal(got, bad) {
		t.Fatalf("addRouteCapability mutated malformed input:\n got %s\nwant %s", got, bad)
	}
	notObj := []byte(`[1,2,3]`)
	if got := addRouteCapability(notObj); !bytes.Equal(got, notObj) {
		t.Fatalf("non-object mutated: %s", got)
	}
}

// unknown method -> decodeFrame succeeds, Raw byte-identical.
func TestUnknownMethodPassthrough(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":"u-1","method":"vendor/custom/thing","params":{"unknown":{"deep":[1,2,3]}}}`)
	f := mustFrame(t, raw)
	if f.Method != "vendor/custom/thing" {
		t.Fatalf("method = %q", f.Method)
	}
	if !bytes.Equal(f.Raw, raw) {
		t.Fatalf("Raw changed for unknown method:\n got %s\nwant %s", f.Raw, raw)
	}
	if isInitializeResult(f) {
		t.Fatal("unknown method classified as initialize result")
	}
}
