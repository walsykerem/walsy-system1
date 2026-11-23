package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// HTTP must emit the same redacted relay audit rows as stdio/unix: one client
// row on read, one server row on reply.
func TestHTTPRelayAuditBothDirections(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "")
	routerReset()
	defer routerReset()

	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`)
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(frame))
	r.Header.Set(mcpProtocolVersionHeader, "2025-06-18")
	rec := httptest.NewRecorder()
	mcpHTTPHandler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	rows := httpRelayRows(t)
	if len(rows) != 2 {
		t.Fatalf("relay rows = %d, want 2: %v", len(rows), rows)
	}
	phases := map[string]bool{}
	for _, row := range rows {
		phase, _ := row["phase"].(string)
		phases[phase] = true
		if row["mode"] != "initialize" || row["status"] != "ok" {
			t.Fatalf("unexpected relay row: %#v", row)
		}
	}
	if !phases["client"] || !phases["server"] {
		t.Fatalf("phases = %v, want client and server", phases)
	}
}

// Malformed JSON gets a 400 but is still recorded as a client frame.
func TestHTTPRelayAuditMalformed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "")
	routerReset()
	defer routerReset()

	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{not-json"))
	rec := httptest.NewRecorder()
	mcpHTTPHandler().ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	rows := httpRelayRows(t)
	for _, row := range rows {
		if row["phase"] == "client" && row["status"] == "malformed" {
			return
		}
	}
	t.Fatalf("malformed client row missing: %v", rows)
}

func httpRelayRows(t *testing.T) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, line := range auditLines(t) {
		var row map[string]any
		if json.Unmarshal([]byte(line), &row) != nil || row["event"] != "relay_frame" {
			continue
		}
		rows = append(rows, row)
	}
	return rows
}
