package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Calls are kept in memory for the life of the process: the LOGS sheet answers
// "how many, how big, when" since start. Payloads are never stored, only
// time, kind, outcome, duration and byte counts. Every call also appends one
// redacted line to the same audit.jsonl the API exposes, so a page refresh
// still sees earlier calls and the browser view and GET /api/logs agree.
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

// auditRow is one redacted line of the shared relay audit JSONL. Only
// allowlisted metadata fields are decoded; keys and payloads never reach here.
type auditRow struct {
	Event  string `json:"event"`
	Method string `json:"method"`
	Mode   string `json:"mode"`
	Status string `json:"status"`
	Count  string `json:"count"`
	Phase  string `json:"phase"`
	Tool   string `json:"tool"`
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
	// Same source as relayLog: only coarse labels and a byte count reach disk.
	routerAudit("call", "method", kind, "status", outcome, "count", strconv.Itoa(reqB))
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

// auditLogPath is the same file routerAudit writes. It is read-only here.
func auditLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, routerDir, routerAuditName), nil
}

// auditLogRows reads the shared audit JSONL newest-first. Unknown fields in a
// line are ignored, so even a hand-edited file cannot expose a key through the
// dashboard or the API.
func auditLogRows() ([]auditRow, error) {
	path, err := auditLogPath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []auditRow{}, nil
		}
		return nil, err
	}
	defer f.Close()
	rows := make([]auditRow, 0)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row auditRow
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
}

// auditLogEntries maps redacted audit rows onto the existing LOGS table, so
// the sheet and GET /api/logs show the same persisted source.
func auditLogEntries(rows []auditRow) (LogTotals, []LogEntry) {
	entries := make([]LogEntry, 0, len(rows))
	var bytes int64
	for _, row := range rows {
		kind := row.Event
		if kind == "" {
			kind = row.Phase
		}
		outcome := row.Status
		name := row.Method
		if name == "" {
			name = row.Mode
		}
		if name == "" {
			name = row.Tool
		}
		count := 0
		_, _ = fmt.Sscan(row.Count, &count)
		bytes += int64(count)
		entries = append(entries, LogEntry{Kind: kind, Outcome: outcome + " " + name,
			ReqB: count})
	}
	return LogTotals{Calls: len(entries), ReqB: bytes, Since: "PERSISTED AUDIT"}, entries
}

// uiLogsSheet renders the LOGS sheet iframe document. A live process shows its
// in-memory call ring (with byte/usage detail); on a fresh load it falls back
// to the persisted audit rows so history survives a restart.
func uiLogsSheet(w http.ResponseWriter) {
	t, entries := logsView()
	if t.Calls == 0 {
		rows, err := auditLogRows()
		if err == nil {
			t, entries = auditLogEntries(rows)
		}
	}
	renderPage(w, uiPage{Title: "Logs", LogsSheetMode: true, Log: t, Logs: entries, Prompt: loadSystemPrompt()})
}
