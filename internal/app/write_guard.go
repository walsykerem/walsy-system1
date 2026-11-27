package app

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
// A non-write call with unparsable arguments fails open, like every other
// guard here; a write with no readable target path fails closed (check).
// shortcut: tool-name verbs, not per-server schemas; upgrade when a client
// ships tools/list input schemas worth parsing.
const (
	guardRepeatLimit = 8
	guardMaxPaths    = 1024
)

var writeVerbs = []string{
	"write", "edit", "create", "patch", "append",
	"delete", "remove", "move", "rename", "mkdir", "rm", "mv",
	"update", "put", "insert", "set", "save",
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

// samePath reports whether a write target (b) names a path this session already
// touched (a). Two full paths match on a shared suffix, so the client's cwd
// never has to match the relay's. In the mixed case only a full-path read
// licenses a bare write: a bare read never licenses a pathed write, which would
// let a write into an unread directory ride on a same-named file elsewhere.
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	sepA, sepB := strings.Contains(a, "/"), strings.Contains(b, "/")
	switch {
	case sepA && sepB:
		return strings.HasSuffix(a, "/"+b) || strings.HasSuffix(b, "/"+a)
	case sepA:
		return strings.HasSuffix(a, "/"+b)
	default:
		return false
	}
}

// check returns a block reason for a write to an unseen path or a runaway
// repeat. Every non-write call just records the paths it named.
func (g *writeGuard) check(tool string, args json.RawMessage) (string, bool) {
	sum := sha256.Sum256(append([]byte(tool+"\x00"), args...))
	key := hex.EncodeToString(sum[:])

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen == nil {
		g.paths, g.seen = map[string]struct{}{}, map[string]int{}
	}
	paths := argumentPaths(args)
	if !isWriteTool(tool) {
		// Record every path the call named, bounded like the write cap.
		for _, path := range paths {
			if len(g.paths) >= guardMaxPaths {
				break
			}
			g.paths[path] = struct{}{}
		}
		return "", false
	}
	if len(paths) == 0 {
		// A write whose target cannot be read is not provably new: block it
		// instead of waving it through.
		return "write with no readable path: name the target path, or state why the path cannot be read", true
	}
	// Only writes carry the runaway-repeat guard: repeating a read is normal.
	// The seen map is capped like paths, so distinct blocked writes never grow
	// it without bound; a key already present keeps counting past the cap.
	if n, ok := g.seen[key]; ok {
		n++
		if n > guardRepeatLimit {
			// Reset so the next attempt is judged on its own, not locked out.
			delete(g.seen, key)
			return "repeated identical call " + strconv.Itoa(guardRepeatLimit) +
				" times: change the arguments or say why it must repeat", true
		}
		g.seen[key] = n
	} else if len(g.seen) < guardMaxPaths {
		g.seen[key] = 1
	}
	// Judge every path the call names: a rename whose dest was never read must
	// block even if its source was.
	for _, path := range paths {
		touched := false
		for seen := range g.paths {
			if samePath(seen, path) {
				touched = true
				break
			}
		}
		if !touched {
			return "write to a path this session never touched: " + path +
				": read it first, or state why it is new", true
		}
	}
	return "", false
}

// guardReasonCode is the short code the audit log can hold; the full text
// stays in the -32001 message.
func guardReasonCode(reason string) string {
	switch {
	case strings.HasPrefix(reason, "write to a path"):
		return "unread-write"
	case strings.HasPrefix(reason, "write with no readable path"):
		return "unread-path"
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
	for _, k := range []string{"content", "new_content", "newContent", "text", "data", "patch", "diff", "body"} {
		if v, ok := m[k].(string); ok {
			return v, true
		}
	}
	return "", false
}

// argumentPath returns the first path-valued argument, for callers that need a
// single target (e.g. the diff precheck).
func argumentPath(args json.RawMessage) string {
	paths := argumentPaths(args)
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}

// argumentPaths returns every path-valued argument in pathArgs order, so a call
// that names several targets (a rename's source and dest) has each one judged
// rather than only the first.
func argumentPaths(args json.RawMessage) []string {
	var m map[string]any
	if json.Unmarshal(args, &m) != nil {
		return nil
	}
	var out []string
	for _, k := range pathArgs {
		if v, ok := m[k].(string); ok && v != "" {
			out = append(out, normalizedPath(v))
		}
	}
	return out
}

// toolSegments splits snake/kebab and camelCase before lowercasing, so
// "write_file"/"writeFile" both yield "write" while "confirm_plan" (which holds
// "rm") yields "confirm"+"plan" only.
func toolSegments(tool string) []string {
	var b strings.Builder
	runes := []rune(tool)
	for i, r := range runes {
		if i > 0 && r >= 'A' && r <= 'Z' &&
			((runes[i-1] >= 'a' && runes[i-1] <= 'z') || (runes[i-1] >= '0' && runes[i-1] <= '9')) {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.FieldsFunc(strings.ToLower(b.String()), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
}

func isWriteTool(tool string) bool {
	for _, seg := range toolSegments(tool) {
		for _, v := range writeVerbs {
			if seg == v {
				return true
			}
		}
	}
	return false
}

// commandVerbs name the tools that run a shell command, so the destructive-arg
// scan only judges those and not an ordinary write whose body mentions "rm".
var commandVerbs = []string{"run", "command", "bash", "shell", "exec", "terminal", "sh", "cmd"}

func isCommandTool(tool string) bool {
	for _, seg := range toolSegments(tool) {
		for _, v := range commandVerbs {
			if seg == v {
				return true
			}
		}
	}
	return false
}
