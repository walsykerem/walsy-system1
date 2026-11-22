package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// writeGuard closes two holes the registry gate cannot see: a write to a path
// this session never touched, and one identical call repeated without limit.
// Both fail open on unparsable arguments, like every other guard here.
// shortcut: tool-name verbs, not per-server schemas; upgrade when a client
// ships tools/list input schemas worth parsing.
const (
	guardRepeatLimit = 8
	guardMaxPaths    = 1024
)

var writeVerbs = []string{
	"write", "edit", "create", "patch", "append",
	"delete", "remove", "move", "rename", "mkdir", "rm", "mv",
}

var pathArgs = []string{
	"path", "file_path", "filePath", "filepath", "file",
	"filename", "target", "source", "dest", "destination",
}

type writeGuard struct {
	mu    sync.Mutex
	paths map[string]struct{}
	seen  map[string]int
}

func newWriteGuard() *writeGuard {
	return &writeGuard{paths: map[string]struct{}{}, seen: map[string]int{}}
}

// toolArgs returns the arguments object of a tools/call frame, or nil.
func toolArgs(params json.RawMessage) json.RawMessage {
	var p struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(params, &p) != nil || len(p.Arguments) == 0 {
		return nil
	}
	return p.Arguments
}

func normalizedPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return filepath.Clean(p)
}

// samePath treats a relative path as equal to any absolute path ending in it,
// so a client's cwd never has to match the relay's.
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	sepA, sepB := strings.Contains(a, "/"), strings.Contains(b, "/")
	if sepA && sepB {
		return strings.HasSuffix(a, "/"+b) || strings.HasSuffix(b, "/"+a)
	}
	return filepath.Base(a) == filepath.Base(b)
}

// check returns a block reason for a write to an unseen path or a runaway
// repeat. Every non-write call just records the paths it named.
func (g *writeGuard) check(tool string, args json.RawMessage) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	sum := sha256.Sum256(append([]byte(tool+"\x00"), args...))
	key := hex.EncodeToString(sum[:])

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen == nil {
		g.paths, g.seen = map[string]struct{}{}, map[string]int{}
	}
	path := argumentPath(args)
	if path == "" {
		return "", false
	}
	if !isWriteTool(tool) {
		if len(g.paths) < guardMaxPaths {
			g.paths[path] = struct{}{}
		}
		return "", false
	}
	// Only writes carry the runaway-repeat guard: repeating a read is normal.
	g.seen[key]++
	if g.seen[key] > guardRepeatLimit {
		// Reset so the next attempt is judged on its own, not locked out.
		delete(g.seen, key)
		return "repeated identical call " + strconv.Itoa(guardRepeatLimit) +
			" times: change the arguments or say why it must repeat", true
	}
	for seen := range g.paths {
		if samePath(seen, path) {
			return "", false
		}
	}
	return "write to a path this session never touched: " + path +
		": read it first, or state why it is new", true
}

// guardReasonCode is the short code the audit log can hold; the full text
// stays in the -32001 message.
func guardReasonCode(reason string) string {
	switch {
	case strings.HasPrefix(reason, "write to a path"):
		return "unread-write"
	case strings.HasPrefix(reason, "repeated identical"):
		return "repeat"
	}
	return "guard"
}

// writeContent returns the new file body a write tool carries, if any, so the
// diff security precheck can scan exactly the text that would be written.
func writeContent(args json.RawMessage) (string, bool) {
	var m map[string]any
	if json.Unmarshal(args, &m) != nil {
		return "", false
	}
	for _, k := range []string{"content", "new_content", "newContent", "text", "data"} {
		if v, ok := m[k].(string); ok {
			return v, true
		}
	}
	return "", false
}

func argumentPath(args json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(args, &m) != nil {
		return ""
	}
	for _, k := range pathArgs {
		if v, ok := m[k].(string); ok && v != "" {
			return normalizedPath(v)
		}
	}
	return ""
}

func isWriteTool(tool string) bool {
	// Split snake/kebab and camelCase before lowercasing: "write_file" and
	// "writeFile" both match, "confirm_plan" (which holds "rm") does not.
	var b strings.Builder
	runes := []rune(tool)
	for i, r := range runes {
		if i > 0 && r >= 'A' && r <= 'Z' &&
			((runes[i-1] >= 'a' && runes[i-1] <= 'z') || (runes[i-1] >= '0' && runes[i-1] <= '9')) {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	for _, seg := range strings.FieldsFunc(strings.ToLower(b.String()), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	}) {
		for _, v := range writeVerbs {
			if seg == v {
				return true
			}
		}
	}
	return false
}
