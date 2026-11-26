package app

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func resetLogs() {
	logMu.Lock()
	logRing, logCalls, logReqB, logRespB, logCredits = nil, 0, 0, 0, 0
	logMu.Unlock()
}

func TestLogsSheetAuditTailBounded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, routerDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var raw strings.Builder
	raw.WriteString(`{"event":"relay_frame","method":"OLDNAV","status":"ok"}` + "\n")
	for i := 0; i < logRingSize+500; i++ {
		raw.WriteString(`{"event":"relay_frame","method":"FILLER","status":"ok"}` + "\n")
	}
	raw.WriteString(`{"event":"relay_frame","method":"NEWMARK","status":"ok"}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, routerAuditName), []byte(raw.String()), 0600); err != nil {
		t.Fatal(err)
	}
	resetLogs()
	defer resetLogs()

	rec := httptest.NewRecorder()
	uiLogsSheet(rec, nil)
	body := rec.Body.String()
	if !strings.Contains(body, "NEWMARK") {
		t.Fatal("newest audit row missing")
	}
	if strings.Contains(body, "OLDNAV") {
		t.Fatal("oldest audit row rendered: tail not bounded")
	}
	if n := strings.Count(body, `class="log-usage"`); n != logRingSize {
		t.Fatalf("rendered rows = %d, want %d", n, logRingSize)
	}
}

func TestLogsSheetRendersRecordedCall(t *testing.T) {
	resetLogs()
	defer resetLogs()
	logCall("systemone", "ok", 12*time.Millisecond, 100, 200, map[string]any{"total_tokens": 7.0})

	rec := httptest.NewRecorder()
	uiLogsSheet(rec, nil)
	body := rec.Body.String()
	for _, want := range []string{"1 CALL(S) SINCE", "systemone", "ok", "total_tokens=7", "100 B SENT", "200 B RECEIVED", `data-ts="`} {
		if !strings.Contains(body, want) {
			t.Fatalf("sheet missing %q", want)
		}
	}
}

func TestLogsSheetDropsFrameworkChatterAndKeepsAuditTime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, routerDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw := strings.Join([]string{
		`{"ts":"1760000000000","event":"call","method":"combo","status":"ok"}`,
		`{"event":"gate","method":"resources/list","reason":"layer0-scope"}`,
		`{"event":"relay_frame","mode":"initialize","status":"ok"}`,
		`{"ts":"1760000001000","event":"decision","method":"system1_decide","decision":"admit"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, routerAuditName), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	resetLogs()
	defer resetLogs()

	rec := httptest.NewRecorder()
	uiLogsSheet(rec, nil)
	body := rec.Body.String()
	for _, want := range []string{`data-ts="1760000001000"`, "admit"} {
		if !strings.Contains(body, want) {
			t.Errorf("sheet missing %q: %s", want, body)
		}
	}
	for _, drop := range []string{"resources/list", "initialize", "layer0-scope", `data-ts="1760000000000"`} {
		if strings.Contains(body, drop) {
			t.Errorf("framework chatter %q still rendered", drop)
		}
	}
	if n := strings.Count(body, `class="log-usage"`); n != 1 {
		t.Fatalf("rendered rows = %d, want 1 (combo probe is hidden)", n)
	}
}

// The ring and the audit file are merged per request, so a newer persisted MCP
// row must not be rendered after an older in-memory call row.
func TestLogsSheetMergesMCPRowsInTimeOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, routerDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	resetLogs()
	defer resetLogs()
	logCall("systemone", "ok", time.Millisecond, 1, 1, nil)
	_, entries := logsView()
	if len(entries) != 1 {
		t.Fatalf("ring entries = %d, want 1", len(entries))
	}
	callTS := entries[0].TS
	later := callTS + 5000
	row := `{"ts":"` + strconv.FormatInt(later, 10) + `","event":"decision","method":"system1_decide","decision":"admit"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, routerAuditName), []byte(row), 0600); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	uiLogsSheet(rec, nil)
	body := rec.Body.String()
	newer := strings.Index(body, `data-ts="`+strconv.FormatInt(later, 10)+`"`)
	older := strings.Index(body, `data-ts="`+strconv.FormatInt(callTS, 10)+`"`)
	if newer < 0 || older < 0 {
		t.Fatalf("rows missing from sheet (newer=%d older=%d): %s", newer, older, body)
	}
	if newer > older {
		t.Fatalf("newer MCP row rendered after older call row (newer=%d older=%d)", newer, older)
	}
}
