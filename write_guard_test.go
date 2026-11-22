package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func guardArgs(p string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"path": p})
	return b
}

func TestWriteGuardBlocksUnreadWrite(t *testing.T) {
	g := newWriteGuard()
	if reason, blocked := g.check("write_file", guardArgs("/tmp/x.txt")); !blocked || reason == "" {
		t.Fatalf("unseen write must block, got %q blocked=%v", reason, blocked)
	}
}

func TestWriteGuardAllowsAfterRead(t *testing.T) {
	g := newWriteGuard()
	if _, blocked := g.check("read_file", guardArgs("/tmp/x.txt")); blocked {
		t.Fatal("read must never block")
	}
	if reason, blocked := g.check("edit_file", guardArgs("/tmp/x.txt")); blocked {
		t.Fatalf("write after read must pass, got %q", reason)
	}
}

func TestWriteGuardMatchesRelativeToAbsolute(t *testing.T) {
	g := newWriteGuard()
	g.check("read_file", guardArgs("/repo/sub/a.go"))
	if _, blocked := g.check("write_file", guardArgs("a.go")); blocked {
		t.Fatal("relative write of a read file must pass")
	}
}

// A bare read (no directory) must not license a write to a same-named file in
// another directory: that write would land in a directory never read. The
// reverse still holds: a full-path read licenses the same file named bare.
func TestWriteGuardBareReadDoesNotLicensePathedWrite(t *testing.T) {
	g := newWriteGuard()
	g.check("read_file", guardArgs("a.go"))
	if _, blocked := g.check("write_file", guardArgs("/repo/sub/a.go")); !blocked {
		t.Fatal("bare read licensed a pathed write into an unread directory")
	}
	h := newWriteGuard()
	h.check("read_file", guardArgs("/repo/sub/a.go"))
	if _, blocked := h.check("write_file", guardArgs("a.go")); blocked {
		t.Fatal("pathed read must license the same file named bare")
	}
}

func TestWriteGuardRepeatLimit(t *testing.T) {
	g := newWriteGuard()
	args := json.RawMessage(`{"path":"/tmp/r.txt","text":"same"}`)
	g.check("read_file", args) // seed the path so writes pass the unread check
	for i := 0; i < guardRepeatLimit; i++ {
		if _, blocked := g.check("write_file", args); blocked {
			t.Fatalf("repeat %d blocked too early", i)
		}
	}
	if reason, blocked := g.check("write_file", args); !blocked || reason == "" {
		t.Fatal("repeat past the limit must block")
	}
	// The counter resets, so the next attempt is judged on its own.
	if _, blocked := g.check("write_file", args); blocked {
		t.Fatal("blocked call must reset the counter")
	}
	// Repeating a read is normal and must never hit the write-only counter.
	for i := 0; i < guardRepeatLimit+2; i++ {
		if _, blocked := g.check("read_file", args); blocked {
			t.Fatalf("read repeat %d blocked", i)
		}
	}
}

// A write whose path cannot be read cannot be checked against what this session
// read, so it is refused; a read naming no path has nothing to record.
func TestWriteGuardRefusesUnreadablePath(t *testing.T) {
	g := newWriteGuard()
	if _, blocked := g.check("read_file", nil); blocked {
		t.Fatal("a read naming no path has nothing to record")
	}
	for _, args := range []json.RawMessage{nil, json.RawMessage(`{"path":42}`), json.RawMessage(`{}`)} {
		if _, blocked := g.check("write_file", args); !blocked {
			t.Fatalf("write with args %s must be refused", args)
		}
	}
}

func TestToolArgsExtractsArguments(t *testing.T) {
	params := json.RawMessage(`{"name":"write_file","arguments":{"path":"/tmp/a"}}`)
	if got := string(toolArgs(params)); got != `{"path":"/tmp/a"}` {
		t.Fatalf("toolArgs = %s", got)
	}
	if toolArgs(json.RawMessage(`{"name":"x"}`)) != nil {
		t.Fatal("missing arguments must be nil")
	}
}

func TestWriteGuardAuditsInAdvisoryMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	d := newDecisionBridge()
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_file","arguments":{"path":"/tmp/advisory.txt"}}}`)
	d.advisoryHooks(frame) // must record, never block or panic
	raw, err := os.ReadFile(filepath.Join(home, ".walsy-system1", "audit.jsonl"))
	if err != nil {
		t.Fatalf("advisory hit must reach the audit log: %v", err)
	}
	if !strings.Contains(string(raw), `"reason":"unread-write"`) {
		t.Fatalf("audit line missing the guard code: %s", raw)
	}
}

// isWriteTool must match whole segments, not substrings: names that merely
// contain a verb ("confirm_plan" holds "rm") are not writes (A7).
func TestIsWriteTool(t *testing.T) {
	for _, name := range []string{
		"confirm_plan", "format_code", "perform_action", "transform",
		"normalize", "dispatch", "readwrite", "warm_cache", "recreate", "rmdir_safe",
	} {
		if isWriteTool(name) {
			t.Errorf("%q must not be a write tool", name)
		}
	}
	for _, name := range []string{
		"write_file", "edit_file", "create_file", "apply_patch", "delete_file",
		"writeFile", "editFile", "deleteFile", "createFile",
		"move", "rename", "mkdir", "write", "edit", "create", "patch", "append",
		"delete", "remove", "move", "rename", "mkdir", "rm", "mv",
	} {
		if !isWriteTool(name) {
			t.Errorf("%q must be a write tool", name)
		}
	}
}

// An emptying write must be judged, not skipped: {"content":""} is a wipe, and
// treating it as "no content" let the destructive case through unscanned.
func TestWriteContentAcceptsEmpty(t *testing.T) {
	body, ok := writeContent(json.RawMessage(`{"path":"/tmp/x","content":""}`))
	if !ok || body != "" {
		t.Fatalf("empty content: body=%q ok=%v want \"\", true", body, ok)
	}
	if _, ok := writeContent(json.RawMessage(`{"path":"/tmp/x"}`)); ok {
		t.Fatal("args without any content key must not be treated as a write")
	}
}

// A body carried under a non-content key (patch/diff/body) must still be
// scanned, or tools that use those keys escape the diff precheck entirely.
func TestWriteContentScansPatchBody(t *testing.T) {
	for _, k := range []string{"patch", "diff", "body"} {
		args := json.RawMessage(`{"path":"/tmp/x","` + k + `":"@@ -1 +1 @@"}`)
		if body, ok := writeContent(args); !ok || body != "@@ -1 +1 @@" {
			t.Errorf("key %q: body=%q ok=%v want the body scanned", k, body, ok)
		}
	}
}

// The seen map is capped like the paths map: distinct writes must not grow it
// without bound.
func TestWriteGuardSeenBounded(t *testing.T) {
	g := newWriteGuard()
	for i := 0; i < guardMaxPaths+64; i++ {
		args := json.RawMessage(`{"path":"/tmp/seen-` + strconv.Itoa(i) + `.txt"}`)
		g.check("write_file", args)
	}
	g.mu.Lock()
	n := len(g.seen)
	g.mu.Unlock()
	if n > guardMaxPaths {
		t.Fatalf("seen grew to %d, cap %d", n, guardMaxPaths)
	}
}

// Every path arg is judged, not only the first: a rename whose dest was never
// read must block even when its source was.
func TestWriteGuardChecksEveryPath(t *testing.T) {
	g := newWriteGuard()
	g.check("read_file", json.RawMessage(`{"path":"/tmp/src.txt"}`))
	rename := json.RawMessage(`{"source":"/tmp/src.txt","dest":"/tmp/dest.txt"}`)
	if _, blocked := g.check("rename_file", rename); !blocked {
		t.Fatal("rename whose dest was never read must block")
	}
	g.check("read_file", json.RawMessage(`{"path":"/tmp/dest.txt"}`))
	if _, blocked := g.check("rename_file", rename); blocked {
		t.Fatal("rename with every path read must pass")
	}
}

// A read-only tool whose name merely contains a write verb ("readwrite") must
// not infer write.unread, while a real write tool still does.
func TestInferItemWriteVerbBoundary(t *testing.T) {
	if got := inferItem("tools/call", "readwrite", "", "docs/plan.md"); got != "" {
		t.Fatalf("readwrite must not infer a write item, got %q", got)
	}
	if got := inferItem("tools/call", "write_file", "", "docs/plan.md"); got != "write.unread" {
		t.Fatalf("write_file must infer write.unread, got %q", got)
	}
}
