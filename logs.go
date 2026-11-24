package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
	Credits float64
}

// LogTotals are the running sums shown above the ring buffer.
type LogTotals struct {
	Calls       int
	SinceTS     int64
	ReqB        int64
	RespB       int64
	Since       string
	CostPerCall float64
}

// auditRow is one redacted line of the shared relay audit JSONL. Only
// allowlisted metadata fields are decoded; keys and payloads never reach here.
type auditRow struct {
	TS       string `json:"ts"`
	Event    string `json:"event"`
	Method   string `json:"method"`
	Mode     string `json:"mode"`
	Status   string `json:"status"`
	Count    string `json:"count"`
	Phase    string `json:"phase"`
	Tool     string `json:"tool"`
	Reason   string `json:"reason,omitempty"`
	Decision string `json:"decision,omitempty"`
	Bucket   string `json:"bucket,omitempty"`
	Allow    string `json:"allow,omitempty"`
	Source   string `json:"source,omitempty"`
	Label    string `json:"label,omitempty"`
	Latency  string `json:"latency_ms,omitempty"`
	Credits  string `json:"credits,omitempty"`
	Provider string `json:"provider,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
}

var (
	logMu      sync.Mutex
	logRing    []LogEntry
	logCalls   int
	logReqB    int64
	logRespB   int64
	logCredits float64
	logSince   = time.Now()
)

// clearLogs empties both views of call history: the in-memory ring and the
// redacted audit file. A missing file is already clear; an existing one is
// replaced atomically so a concurrent reader never sees a half-truncated JSONL.
func clearLogs() error {
	// Hold the lock through both clears: a concurrent logCall must not append
	// to the ring after it was emptied but before the audit file is truncated.
	logMu.Lock()
	defer logMu.Unlock()
	logRing = nil
	logCalls, logReqB, logRespB, logCredits = 0, 0, 0, 0
	logSince = time.Now()
	path, err := auditLogPath()
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return writeStateFile(path, nil)
}

// usageCredits returns numeric credits from an SO usage object, if present.
func usageCredits(usage map[string]any) (float64, bool) {
	if c, ok := usage["credits"].(float64); ok {
		return c, true
	}
	for _, v := range usage {
		if m, ok := v.(map[string]any); ok {
			if c, ok := m["credits"].(float64); ok {
				return c, true
			}
		}
	}
	return 0, false
}

// logCall records one outbound call. usage is an optional API usage object.
func logCall(kind, outcome string, d time.Duration, reqB, respB int, usage map[string]any) {
	now := time.Now()
	credits, _ := usageCredits(usage) // credits stay numeric in RAM only
	e := LogEntry{
		TS:      now.UnixMilli(),
		At:      now.In(time.Local).Format("15:04:05"),
		Kind:    kind,
		Outcome: outcome,
		MS:      d.Milliseconds(),
		ReqB:    reqB,
		RespB:   respB,
		Usage:   usageLine(usage),
		Credits: credits,
	}
	logMu.Lock()
	defer logMu.Unlock()
	logCalls++
	logReqB += int64(reqB)
	logRespB += int64(respB)
	logCredits += credits
	logRing = append(logRing, e)
	if len(logRing) > logRingSize {
		logRing = logRing[len(logRing)-logRingSize:]
	}
	creditStr := strconv.FormatFloat(credits, 'f', -1, 64) // numeric amount only; never the usage object, key or answer
	// Same source as relayLog: only coarse labels and numeric totals reach disk.
	kv := []string{"method", kind, "status", outcome, "count", strconv.Itoa(reqB),
		"latency_ms", strconv.FormatInt(d.Milliseconds(), 10), "credits", creditStr}
	if kind == "systemone" {
		kv = append(kv, "source", "so")
	}
	routerAudit("call", kv...)
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
	if logCalls > 0 {
		t.CostPerCall = logCredits / float64(logCalls)
	}
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

// auditLogRowsTail returns at most limit newest rows, reading only the file's
// tail. The UI opens this on every sheet click; a full scan of a large audit
// file made the LOGS sheet render megabytes of rows and stall the click.
func auditLogRowsTail(limit int) ([]auditRow, error) {
	if limit <= 0 {
		return []auditRow{}, nil
	}
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
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("audit file rejected")
	}
	const tailBytes = 1 << 20
	var off int64
	if st.Size() > tailBytes {
		off = st.Size() - tailBytes
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, tailBytes))
	if err != nil {
		return nil, err
	}
	if off > 0 {
		// Seek may start mid-line; only lines after the first newline are whole.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	lines := bytes.Split(data, []byte{'\n'})
	rows := make([]auditRow, 0, limit)
	for i := len(lines) - 1; i >= 0 && len(rows) < limit; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var row auditRow
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		rows = append(rows, row)
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
		name := row.Method
		if name == "" {
			name = row.Mode
		}
		if name == "" {
			name = row.Tool
		}
		if name == "" {
			name = row.Provider
		}
		// MCP decision rows carry no status/method; surface their redacted
		// metadata instead of rendering an empty outcome cell.
		outcome := strings.TrimSpace(strings.Join([]string{
			row.Status, row.Reason, row.Decision, row.Bucket, row.Allow,
			row.Source, row.Label, row.Outcome,
		}, " "))
		if name != "" {
			outcome = strings.TrimSpace(name + " " + outcome)
		}
		count := 0
		_, _ = fmt.Sscan(row.Count, &count)
		bytes += int64(count)
		ms := int64(0)
		_, _ = fmt.Sscan(row.Latency, &ms)
		ts, _ := strconv.ParseInt(row.TS, 10, 64)
		at := "-"
		if ts > 0 {
			at = time.UnixMilli(ts).In(time.Local).Format("15:04:05")
		}
		entries = append(entries, LogEntry{TS: ts, At: at, Kind: kind, Outcome: outcome, ReqB: count, MS: ms})
	}
	return LogTotals{Calls: len(entries), ReqB: bytes, Since: "PERSISTED AUDIT"}, entries
}

// auditSignalRows drops MCP transport chatter (capability probes, relay frames,
// provider pinning notices) from the sheet. Calls and decisions are the rows a
// reader acts on; raw chatter stays available through GET /api/logs.
func auditSignalRows(rows []auditRow) []auditRow {
	out := make([]auditRow, 0, len(rows))
	for _, row := range rows {
		switch row.Event {
		case "call", "decision", "drift_alarm", "writeguard", "diffguard":
			out = append(out, row)
		case "gate":
			if row.Tool != "" || row.Decision != "" || row.Label != "" {
				out = append(out, row)
			}
		case "relay_frame":
			if row.Mode == "tools/call" || row.Method != "" {
				out = append(out, row)
			}
		}
	}
	return out
}

// uiLogsSheet renders the LOGS sheet iframe document. The in-memory call ring
// keeps byte/usage detail; persisted MCP decision rows are merged in so one
// sheet covers both, and a fresh load falls back to audit rows entirely.
func uiLogsSheet(w http.ResponseWriter, r *http.Request) {
	t, entries := logsView()
	rows, err := auditLogRowsTail(logRingSize * 4)
	if err == nil {
		rows = auditSignalRows(rows)
		if len(rows) > logRingSize {
			rows = rows[:logRingSize]
		}
		if t.Calls == 0 {
			t, entries = auditLogEntries(rows)
		} else {
			// The ring holds call rows only; MCP decisions write straight to
			// audit.jsonl. Merge those so one LOGS sheet shows both transports.
			mcp := make([]auditRow, 0, len(rows))
			for _, row := range rows {
				if row.Event != "call" {
					mcp = append(mcp, row)
				}
			}
			_, extra := auditLogEntries(mcp)
			entries = append(entries, extra...)
		}
	}
	msg, msgMs := "", 0
	if r != nil {
		msg = r.URL.Query().Get("msg")
		msgMs = uiMsgMs(r)
	}
	renderPage(w, uiPage{Title: "Logs", LogsSheetMode: true, Log: t, Logs: entries, Msg: msg, MsgMs: msgMs})
}
