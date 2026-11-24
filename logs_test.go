package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetLogs() {
	logMu.Lock()
	logRing, logCalls, logReqB, logRespB, logCredits = nil, 0, 0, 0, 0
	logMu.Unlock()
}

func TestLogCallCapsRingAndCounts(t *testing.T) {
	resetLogs()
	defer resetLogs()

	for i := 0; i < logRingSize+25; i++ {
		logCall("systemone", "ok", time.Millisecond, 10, 20, nil)
	}
	total, entries := logsView()
	if total.Calls != logRingSize+25 {
		t.Fatalf("calls = %d, want %d", total.Calls, logRingSize+25)
	}
	if len(entries) != logRingSize {
		t.Fatalf("ring = %d, want %d", len(entries), logRingSize)
	}
	if total.ReqB != int64(10*(logRingSize+25)) || total.RespB != int64(20*(logRingSize+25)) {
		t.Fatalf("bytes = %d/%d", total.ReqB, total.RespB)
	}
}

func TestUsageLineFlattensNumbers(t *testing.T) {
	got := usageLine(map[string]any{"total_tokens": 42.0, "nested": map[string]any{"b": 2.0}, "name": "x"})
	if !strings.Contains(got, "nested.b=2") || !strings.Contains(got, "total_tokens=42") {
		t.Fatalf("usageLine = %q", got)
	}
	if strings.Contains(got, "name") {
		t.Fatalf("non-numeric leaked: %q", got)
	}
}

// A large persisted audit must not be fully read and rendered on every sheet
// open: only the newest logRingSize rows reach the browser.
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

func TestLogTimestampsUseLocalClock(t *testing.T) {
	resetLogs()
	defer resetLogs()
	before := time.Now()
	logCall("systemone", "ok", time.Second, 1, 1, nil)
	total, entries := logsView()
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	ok := entries[0].At == before.In(time.Local).Format("15:04:05") ||
		entries[0].At == time.Now().In(time.Local).Format("15:04:05")
	if !ok {
		t.Fatalf("At %q is not the local clock %q", entries[0].At, before.In(time.Local).Format("15:04:05"))
	}
	if want := logSince.In(time.Local).Format("15:04:05"); total.Since != want {
		t.Fatalf("Since %q not process start %q", total.Since, want)
	}
}
