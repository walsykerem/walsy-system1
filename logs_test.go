package main

import (
	"net/http/httptest"
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

func TestLogsSheetRendersRecordedCall(t *testing.T) {
	resetLogs()
	defer resetLogs()
	logCall("systemone", "ok", 12*time.Millisecond, 100, 200, map[string]any{"total_tokens": 7.0})

	rec := httptest.NewRecorder()
	uiLogsSheet(rec)
	body := rec.Body.String()
	for _, want := range []string{"1 CALL(S) SINCE", "systemone", "ok", "total_tokens=7", "100 B SENT", "200 B RECEIVED", `data-ts="`} {
		if !strings.Contains(body, want) {
			t.Fatalf("sheet missing %q", want)
		}
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
	// Accept the minute before the call too: the test can straddle :00.
	if want, prev := time.Now().In(time.Local).Format("15:04"), before.In(time.Local).Format("15:04"); total.Since[:5] != want && total.Since[:5] != prev {
		t.Fatalf("Since %q not local now %q", total.Since, want)
	}
}
