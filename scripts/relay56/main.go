// Command relay56 drives the shipped relay binary over stdio and asserts the
// refusals it must make, with a local fake decision endpoint so every label is
// deterministic. Upstream is `cat`, which echoes each forwarded frame back, so
// a pass-through is observable instead of inferred from silence.
//
// Kept in the repo: an evidence harness under /tmp is deleted by a restart and
// then the claims it backed cannot be replayed.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var bin = func() string {
	if v := os.Getenv("RELAY56_BIN"); v != "" {
		return v
	}
	return "/Users/keremurhan/walsy-system1/walsy-system1"
}()

type frame struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type client struct {
	in   io.WriteCloser
	r    *bufio.Reader
	cmd  *exec.Cmd
	home string
}

var (
	mu    sync.Mutex
	queue []answer
	fail  int
)

type answer struct {
	Choice string  `json:"choice"`
	Conf   float64 `json:"confidence"`
}

func script(a ...answer) {
	mu.Lock()
	defer mu.Unlock()
	queue = a
}

func next() answer {
	mu.Lock()
	defer mu.Unlock()
	if len(queue) == 0 {
		return answer{Choice: "yes", Conf: 0.95}
	}
	a := queue[0]
	queue = queue[1:]
	return a
}

// endpoint is the fake System One: it answers the shape gate.go parses.
func endpoint() string {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		a := next()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answers": map[string]any{"gate": map[string]any{
				"choice": a.Choice, "confidence": a.Conf,
			}},
		})
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		die(err)
	}
	go func() { _ = http.Serve(l, mux) }()
	return "http://" + l.Addr().String() + "/v1/systemone"
}

func die(err error) {
	fmt.Println("FAIL harness:", err)
	os.Exit(2)
}

func spawn(ep string) *client {
	// A private HOME isolates the audit log so checkReason can read back the
	// exact reason code the relay recorded without touching the user's own.
	home, err := os.MkdirTemp("", "relay56-home-")
	if err != nil {
		die(err)
	}
	cmd := exec.Command(bin, "cat")
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"WALSY_SO_ENDPOINT="+ep,
		"EVREN_API_KEY=relay56-test-key",
		"WALSY_SO_MODE=enforcing",
		"WALSY_ROUTER_RETRY_BUDGET=3",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		die(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		die(err)
	}
	if err := cmd.Start(); err != nil {
		die(err)
	}
	return &client{in: stdin, r: bufio.NewReader(stdout), cmd: cmd, home: home}
}

func (c *client) close() {
	c.in.Close()
	_ = c.cmd.Wait()
	_ = os.RemoveAll(c.home)
}

// send writes one frame and waits for the reply with the same id, skipping the
// relay's own metric lines.
func (c *client) send(f frame) map[string]any {
	b, _ := json.Marshal(f)
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		die(err)
	}
	done := make(chan map[string]any, 1)
	go func() {
		for {
			line, err := c.r.ReadString('\n')
			if err != nil {
				done <- nil
				return
			}
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) != nil {
				continue
			}
			if v, ok := m["id"].(float64); ok && int(v) == f.ID {
				done <- m
				return
			}
		}
	}()
	select {
	case m := <-done:
		return m
	case <-time.After(10 * time.Second):
		return nil
	}
}

func msg(m map[string]any) string {
	if m == nil {
		return ""
	}
	if e, ok := m["error"].(map[string]any); ok {
		s, _ := e["message"].(string)
		return s
	}
	b, _ := json.Marshal(m["result"])
	return string(b)
}

// check fails when any wanted substring is missing, and fails on an empty want
// so a forgotten assertion can never print PASS.
func check(name, got string, want ...string) {
	if len(want) == 0 {
		fmt.Printf("FAIL %-14s no assertion given\n", name)
		fail++
		return
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			fmt.Printf("FAIL %-14s missing %q in %.120s\n", name, w, got)
			fail++
			return
		}
	}
	fmt.Printf("PASS %-14s %.110s\n", name, got)
}

// checkForward asserts the call was passed through unchanged: the answer must
// be the upstream echo of the same tools/call with the same argument, and must
// carry no error. A blocked call answers with an error instead, so it fails.
func checkForward(name string, m map[string]any, wantArg string) {
	if m == nil {
		fmt.Printf("FAIL %-14s no reply\n", name)
		fail++
		return
	}
	if e, ok := m["error"]; ok && e != nil {
		fmt.Printf("FAIL %-14s blocked: %.110s\n", name, msg(m))
		fail++
		return
	}
	if m["method"] != "tools/call" {
		fmt.Printf("FAIL %-14s not a forwarded call: %.110s\n", name, msg(m))
		fail++
		return
	}
	b, _ := json.Marshal(m["params"])
	if !strings.Contains(string(b), wantArg) {
		fmt.Printf("FAIL %-14s forwarded args lack %q: %.110s\n", name, wantArg, b)
		fail++
		return
	}
	fmt.Printf("PASS %-14s forwarded unchanged (%s)\n", name, wantArg)
}

// checkDisk asserts the refused text never landed, so "the write was refused"
// is measured on disk instead of inferred from the reply.
func checkDisk(name, path, forbidden string) {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("PASS %-14s on disk: absent (%v)\n", name, err)
		return
	}
	if strings.Contains(string(b), forbidden) {
		fmt.Printf("FAIL %-14s on disk: %q present in %s\n", name, forbidden, path)
		fail++
		return
	}
	fmt.Printf("PASS %-14s on disk: present without %q (%d B)\n", name, forbidden, len(b))
}

// checkReason asserts the relay audited a decision carrying the given reason
// code. The audit JSONL is the only observable that names a reason: a forwarded
// frame's reply is the upstream echo and never carries the code.
func checkReason(name, home, reason string) {
	b, err := os.ReadFile(filepath.Join(home, ".walsy-system1", "audit.jsonl"))
	if err != nil {
		fmt.Printf("FAIL %-18s no audit log: %v\n", name, err)
		fail++
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if m["reason"] == reason {
			fmt.Printf("PASS %-18s audited reason=%s\n", name, reason)
			return
		}
	}
	fmt.Printf("FAIL %-18s reason %q not in audit\n", name, reason)
	fail++
}

func call(id int, tool string, args map[string]any) frame {
	p, _ := json.Marshal(map[string]any{"name": tool, "arguments": args})
	return frame{ID: id, Method: "tools/call", Params: p}
}

func handshake(c *client, id int) { c.send(frame{ID: id, Method: "initialize"}) }

func main() {
	ep := endpoint()

	// 1) a write to a path this session never read is refused.
	c := spawn(ep)
	handshake(c, 1)
	check("unread-write", msg(c.send(call(2, "write_file", map[string]any{
		"path": "/tmp/relay56-a.txt", "content": "x",
	}))), "never touched", "blocked")
	c.close()

	// 2) after the path is read, the same write is refused for the secret it
	// would add, and the file is measured on disk.
	c = spawn(ep)
	handshake(c, 1)
	_ = os.Remove("/tmp/relay56-b.txt")
	checkForward("read-ok", c.send(call(2, "read_file", map[string]any{"path": "/tmp/relay56-b.txt"})), "/tmp/relay56-b.txt")
	check("diff-secret", msg(c.send(call(3, "write_file", map[string]any{
		"path":    "/tmp/relay56-b.txt",
		"content": "-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----\n",
	}))), "keep the secret", "blocked")
	checkDisk("secret-absent", "/tmp/relay56-b.txt", "PRIVATE KEY")
	c.close()

	// 2b) a call with no file body at all is still scanned: a shell redirect
	// carrying the same key shape is refused at the argument level.
	c = spawn(ep)
	handshake(c, 1)
	check("args-secret", msg(c.send(call(2, "run_command", map[string]any{
		"command": "echo '-----BEGIN RSA PRIVATE KEY-----' > /tmp/relay56-e.txt",
	}))), "keep the secret", "blocked")
	checkDisk("args-absent", "/tmp/relay56-e.txt", "PRIVATE KEY")
	c.close()

	// 3) an emptying write on a file that has content is refused, which only
	// happens when the gate reads the old content from disk.
	wipe := "/tmp/relay56-w.txt"
	if err := os.WriteFile(wipe, []byte("keep this line\n"), 0644); err != nil {
		die(err)
	}
	c = spawn(ep)
	handshake(c, 1)
	checkForward("wipe-read", c.send(call(2, "read_file", map[string]any{"path": wipe})), wipe)
	check("diff-wipe", msg(c.send(call(3, "write_file", map[string]any{"path": wipe, "content": ""}))), "blocked")
	if b, err := os.ReadFile(wipe); err != nil || string(b) != "keep this line\n" {
		fmt.Printf("FAIL wipe-kept     file changed: %q %v\n", b, err)
		fail++
	} else {
		fmt.Println("PASS wipe-kept     content still on disk")
	}
	c.close()

	// 3b) a write that removes most of a file without adding anything is the
	// destructive arm, also judged against what is on disk.
	del := "/tmp/relay56-d.txt"
	body := ""
	for i := 0; i < 25; i++ {
		body += fmt.Sprintf("line %d\n", i)
	}
	if err := os.WriteFile(del, []byte(body), 0644); err != nil {
		die(err)
	}
	c = spawn(ep)
	handshake(c, 1)
	checkForward("del-read", c.send(call(2, "read_file", map[string]any{"path": del})), del)
	check("diff-destructive", msg(c.send(call(3, "write_file", map[string]any{"path": del, "content": "line 0\n"}))), "blocked")
	if b, err := os.ReadFile(del); err != nil || string(b) != body {
		fmt.Printf("FAIL del-kept       file changed: %d B %v\n", len(b), err)
		fail++
	} else {
		fmt.Println("PASS del-kept       content still on disk")
	}
	c.close()

	// 4) a generic "no" label passes the call through unchanged.
	c = spawn(ep)
	handshake(c, 1)
	script(answer{Choice: "no", Conf: 0.96})
	checkForward("generic-no", c.send(call(2, "read_file", map[string]any{"path": "/tmp/relay56-c.txt"})), "/tmp/relay56-c.txt")
	c.close()

	// 5) the third tool call spends the last token, the fourth is refused, and
	// initialize opens a new session with a fresh budget.
	c = spawn(ep)
	handshake(c, 1)
	for i := 2; i <= 4; i++ {
		checkForward(fmt.Sprintf("budget-%d", i), c.send(call(i, "read_file", map[string]any{
			"path": fmt.Sprintf("/tmp/relay56-%d.txt", i),
		})), fmt.Sprintf("/tmp/relay56-%d.txt", i))
	}
	check("budget-stop", msg(c.send(call(5, "read_file", map[string]any{"path": "/tmp/relay56-5.txt"}))), "budget spent", "initialize")
	handshake(c, 6)
	checkForward("budget-refill", c.send(call(7, "read_file", map[string]any{"path": "/tmp/relay56-7.txt"})), "/tmp/relay56-7.txt")
	c.close()

	// 6) so-no: a generic frame the model labels "no" is not an admit, and the
	// typed layer fails open (forwards) while recording the reason code.
	c = spawn(ep)
	handshake(c, 1)
	script(answer{Choice: "no", Conf: 0.96})
	checkForward("so-no", c.send(call(2, "read_file", map[string]any{"path": "/tmp/relay56-n.txt"})), "/tmp/relay56-n.txt")
	checkReason("so-no-audit", c.home, "so-no")
	c.close()

	// 7) so-other: an "other" label is likewise not an admit and fails open.
	c = spawn(ep)
	handshake(c, 1)
	script(answer{Choice: "other", Conf: 0.95})
	checkForward("so-other", c.send(call(2, "read_file", map[string]any{"path": "/tmp/relay56-o.txt"})), "/tmp/relay56-o.txt")
	checkReason("so-other-audit", c.home, "so-other")
	c.close()

	// 8) no-state: a ledger frame with no verdict text routes to no-state and
	// is forwarded with the matching steer.
	c = spawn(ep)
	handshake(c, 1)
	checkForward("no-state", c.send(frame{ID: 2, Method: "tools/call",
		Params: json.RawMessage(`{"item":"antishirk.todo_ledger","state":"x"}`)}), "antishirk.todo_ledger")
	checkReason("no-state-audit", c.home, "no-state")
	c.close()

	// 9) layer0-static: a bare static word reaches the Layer 0 short-circuit
	// instead of the model, and is answered without a decision call.
	c = spawn(ep)
	handshake(c, 1)
	checkForward("layer0-static", c.send(frame{ID: 2, Method: "tools/call",
		Params: json.RawMessage(`{"name":"read_file","state":"status","arguments":{"path":"/tmp/relay56-s.txt"}}`)}), "/tmp/relay56-s.txt")
	checkReason("layer0-static-audit", c.home, "layer0-static")
	c.close()

	if fail > 0 {
		fmt.Printf("FAILED %d checks\n", fail)
		os.Exit(1)
	}
	fmt.Println("RELAY-OK")
}
