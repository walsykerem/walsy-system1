package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Calls are kept in memory for the life of the process: the LOGS sheet answers
// "how many, how big, when" since start. Payloads are never stored, only
// time, kind, outcome, duration and byte counts.
const logRingSize = 200

// LogEntry is one outbound call, display-ready.
type LogEntry struct {
	TS      int64 // unix millis; the sheet renders this in the viewer's zone
	At      string
	Kind    string
	Outcome string
	MS      int64
	ReqB    int
	RespB   int
	Usage   string
}

// LogTotals are the running sums shown above the ring buffer.
type LogTotals struct {
	Calls   int
	SinceTS int64
	ReqB    int64
	RespB   int64
	Since   string
}

var (
	logMu    sync.Mutex
	logRing  []LogEntry
	logCalls int
	logReqB  int64
	logRespB int64
	logSince = time.Now()
)

// logCall records one outbound call. usage is an optional API usage object.
func logCall(kind, outcome string, d time.Duration, reqB, respB int, usage map[string]any) {
	now := time.Now()
	e := LogEntry{
		TS:      now.UnixMilli(),
		At:      now.In(time.Local).Format("15:04:05"),
		Kind:    kind,
		Outcome: outcome,
		MS:      d.Milliseconds(),
		ReqB:    reqB,
		RespB:   respB,
		Usage:   usageLine(usage),
	}
	logMu.Lock()
	defer logMu.Unlock()
	logCalls++
	logReqB += int64(reqB)
	logRespB += int64(respB)
	logRing = append(logRing, e)
	if len(logRing) > logRingSize {
		logRing = logRing[len(logRing)-logRingSize:]
	}
}

// usageLine flattens an API usage object to "key=value" pairs, sorted and
// capped so one exotic response cannot stretch the row.
func usageLine(usage map[string]any) string {
	if len(usage) == 0 {
		return ""
	}
	parts := make([]string, 0, 6)
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if len(parts) >= 6 {
				return
			}
			switch v := m[k].(type) {
			case float64:
				parts = append(parts, fmt.Sprintf("%s%s=%g", prefix, k, v))
			case map[string]any:
				walk(prefix+k+".", v)
			}
		}
	}
	walk("", usage)
	return strings.Join(parts, " ")
}

// logsView returns totals and the ring newest-first.
func logsView() (LogTotals, []LogEntry) {
	logMu.Lock()
	defer logMu.Unlock()
	t := LogTotals{Calls: logCalls, ReqB: logReqB, RespB: logRespB,
		SinceTS: logSince.UnixMilli(), Since: logSince.In(time.Local).Format("15:04:05")}
	out := make([]LogEntry, len(logRing))
	for i, e := range logRing {
		out[len(logRing)-1-i] = e
	}
	return t, out
}

// uiLogsSheet renders the LOGS sheet iframe document.
func uiLogsSheet(w http.ResponseWriter) {
	t, entries := logsView()
	renderPage(w, uiPage{Title: "Logs", LogsSheetMode: true, Log: t, Logs: entries})
}
