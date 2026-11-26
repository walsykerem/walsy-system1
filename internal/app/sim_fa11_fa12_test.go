package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// w30_probe is a real HTTP SO endpoint. It records every POST body and returns
// a reply chosen by the caller, so each simulation exercises the real transport,
// parser, router and cache rather than a stubbed decision function.
type w30_probe struct {
	posts  atomic.Int64
	mu     sync.Mutex
	bodies []string
	reply  func(map[string]any) string
}

func (p *w30_probe) install(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		p.mu.Lock()
		p.bodies = append(p.bodies, string(b))
		p.mu.Unlock()
		p.posts.Add(1)
		var req map[string]any
		_ = json.Unmarshal(b, &req)
		reply := w30_choiceReply(req)
		if p.reply != nil {
			reply = p.reply(req)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	oldEndpoint, oldClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	t.Cleanup(func() {
		soEndpoint, soClient = oldEndpoint, oldClient
		srv.Close()
	})
}

func (p *w30_probe) body(i int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i < 0 || i >= len(p.bodies) {
		return ""
	}
	return p.bodies[i]
}

func w30_env(t *testing.T) {
	t.Helper()
	resetRouterForTest(t)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("EVREN_API_KEY", "sim-fa-key")
}

func w30_frame(method string, id int, fields string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) +
		`,"method":"` + method + `","params":{` + explicitCaps + fields + `}}`)
}

func w30_itemFrame(method string, id int, item, state string) []byte {
	return w30_frame(method, id, `,"item":"`+item+`","state":"`+state+`"`)
}

func w30_labels(req map[string]any) []string {
	qs, _ := req["questions"].(map[string]any)
	gate, _ := qs["gate"].(map[string]any)
	crit, _ := gate["criteria"].(map[string]any)
	out := make([]string, 0, len(crit))
	for k := range crit {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// w30_winner reads the real registry's proceed list out of the item id carried
// in the SO state, so the stub answers like a well-behaved oracle would.
func w30_winner(req map[string]any, labels []string) string {
	state, _ := req["state"].(string)
	var skel StateSkeleton
	if json.Unmarshal([]byte(state), &skel) == nil {
		if it, ok := decisionItemFor(skel.Item); ok && len(it.Proceed) > 0 {
			return it.Proceed[0]
		}
	}
	if len(labels) > 0 {
		return labels[0]
	}
	return "yes"
}

func w30_choiceReply(req map[string]any) string {
	labels := w30_labels(req)
	if len(labels) == 0 {
		labels = []string{"yes", "no", "other"}
	}
	winner := w30_winner(req, labels)
	probs := make(map[string]float64, len(labels))
	probs[winner] = 0.9
	for _, l := range labels {
		if l != winner {
			probs[l] = 0.1 / float64(len(labels)-1)
		}
	}
	b, _ := json.Marshal(map[string]any{"answers": map[string]any{"gate": map[string]any{
		"type": "choice", "choice": winner, "confidence": 0.95, "probabilities": probs,
	}}})
	return string(b)
}

func w30_scoreReply(req map[string]any) string {
	labels := w30_labels(req)
	if len(labels) == 0 {
		labels = []string{"yes", "no", "other"}
	}
	winner := w30_winner(req, labels)
	probs := make(map[string]float64, len(labels))
	probs[winner] = 0.9
	for _, l := range labels {
		if l != winner {
			probs[l] = 0.1 / float64(len(labels)-1)
		}
	}
	b, _ := json.Marshal(map[string]any{"answers": map[string]any{"gate": map[string]any{
		"type": "score", "choice": winner, "score": 0.9, "confidence": 0.9,
		"probabilities": probs,
	}}})
	return string(b)
}

func w30_lines(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func w30_normKey(k string) string {
	k = strings.ToLower(k)
	k = strings.ReplaceAll(k, "_", "")
	k = strings.ReplaceAll(k, "-", "")
	return k
}

func w30_findTrace(v any) (map[string]any, bool) {
	targets := []string{"strategy", "tier", "comparable", "scorekind", "decidedby", "winner", "runnerup", "reason"}
	switch x := v.(type) {
	case map[string]any:
		norm := make(map[string]any, len(x))
		for k, val := range x {
			norm[w30_normKey(k)] = val
		}
		hit := true
		for _, t := range targets {
			if _, ok := norm[t]; !ok {
				hit = false
				break
			}
		}
		if hit {
			return x, true
		}
		for _, val := range x {
			if tr, ok := w30_findTrace(val); ok {
				return tr, true
			}
		}
	case []any:
		for _, val := range x {
			if tr, ok := w30_findTrace(val); ok {
				return tr, true
			}
		}
	}
	return nil, false
}

func TestSimFA11(t *testing.T) {
	t.Run("typed_score_full_probabilities", func(t *testing.T) {
		w30_env(t)
		p := &w30_probe{reply: w30_scoreReply}
		p.install(t)
		br := newDecisionBridge()
		r := br.decideFrame(w30_itemFrame("tools/call", 1, "reasoning.effort", "classify effort"))
		if r.Reason == "so-degraded" || r.Decision != decPin {
			t.Errorf("BROKEN FA11 typed score: decision=%q reason=%q", r.Decision, r.Reason)
		}
		var sent map[string]any
		if err := json.Unmarshal([]byte(p.body(0)), &sent); err != nil {
			t.Fatalf("SO POST is not JSON: %v", err)
		}
		labels := w30_labels(sent)
		dec := r.LastDecision
		if dec.AnswerType != "score" {
			t.Errorf("BROKEN FA11 typed score: AnswerType=%q want score", dec.AnswerType)
		}
		if dec.Score != 0.9 {
			t.Errorf("BROKEN FA11 typed score: Score=%v want 0.9", dec.Score)
		}
		if len(dec.Probabilities) != len(labels) {
			t.Errorf("BROKEN FA11 full probabilities: got %d entries %v, want %d (%v)",
				len(dec.Probabilities), dec.Probabilities, len(labels), labels)
		}
		winner := w30_winner(sent, labels)
		for _, l := range labels {
			want := 0.1 / float64(len(labels)-1)
			if l == winner {
				want = 0.9
			}
			if got := dec.Probabilities[l]; got < want-1e-9 || got > want+1e-9 {
				t.Errorf("BROKEN FA11 probability %q=%v want %v", l, got, want)
			}
		}
	})

	t.Run("typed_noul_explicit_abstain", func(t *testing.T) {
		w30_env(t)
		p := &w30_probe{reply: func(map[string]any) string {
			return `{"answers":{"gate":{"type":"noul","noul":true,"confidence":0.99}}}`
		}}
		p.install(t)
		br := newDecisionBridge()
		r := br.decideFrame(w30_itemFrame("initialize", 1, "reasoning.effort", "classify effort"))
		if r.LastDecision.AnswerType != "noul" {
			t.Errorf("BROKEN FA11 typed noul: AnswerType=%q want noul", r.LastDecision.AnswerType)
		}
		if r.Reason == "so-degraded" {
			t.Error("BROKEN FA11 typed noul: explicit no-answer collapsed into so-degraded")
		}
		if !r.Abstained {
			t.Errorf("BROKEN FA11 typed noul: not an abstain (%+v)", r)
		}
	})

	t.Run("ranking_trace_slog_json", func(t *testing.T) {
		w30_env(t)
		p := &w30_probe{}
		p.install(t)
		var metricBuf bytes.Buffer
		oldMetric := metricOut
		metricOut = &metricBuf
		t.Cleanup(func() { metricOut = oldMetric })
		var slogBuf bytes.Buffer
		oldLog := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&slogBuf, nil)))
		t.Cleanup(func() { slog.SetDefault(oldLog) })

		br := newDecisionBridge()
		r := br.decideFrame(w30_itemFrame("initialize", 1, "reasoning.effort", "classify effort"))
		if r.Decision != decPin {
			t.Fatalf("setup failed: decision=%q reason=%q", r.Decision, r.Reason)
		}
		var found map[string]any
		for _, line := range append(w30_lines(metricBuf.String()), w30_lines(slogBuf.String())...) {
			var rec any
			if json.Unmarshal([]byte(line), &rec) != nil {
				continue
			}
			if tr, ok := w30_findTrace(rec); ok {
				found = tr
				break
			}
		}
		if found == nil {
			t.Errorf("BROKEN FA11 RankingTrace: no 8-field trace JSON in metric or slog output (metric=%q slog=%q)",
				metricBuf.String(), slogBuf.String())
			return
		}
		for k := range found {
			n := w30_normKey(k)
			if n == "confidence" || n == "probabilities" || n == "score" || n == "conf" {
				t.Errorf("BROKEN FA11 RankingTrace: raw score field %q leaked into trace", k)
			}
		}
	})

	t.Run("model_effort_route_separate_decisions", func(t *testing.T) {
		w30_env(t)
		p := &w30_probe{}
		p.install(t)
		br := newDecisionBridge()
		items := []string{"reasoning.effort", "reasoning.model_route"}
		for i, id := range items {
			r := br.decideFrame(w30_itemFrame("initialize", i+1, id, "classify "+id))
			if r.Decision != decPin {
				t.Fatalf("setup failed for %s: decision=%q reason=%q", id, r.Decision, r.Reason)
			}
			var sent map[string]any
			if json.Unmarshal([]byte(p.body(i)), &sent) != nil {
				t.Fatalf("POST %d is not JSON", i)
			}
			t.Logf("%s labels=%v", id, w30_labels(sent))
		}
		if p.posts.Load() != 2 {
			t.Errorf("BROKEN FA11 separation: %d SO POSTs for 2 items, want 2", p.posts.Load())
		}
		if len(w30_labels(w30_mustJSON(t, p.body(0)))) == 0 ||
			len(w30_labels(w30_mustJSON(t, p.body(1)))) == 0 {
			t.Error("BROKEN FA11 separation: missing label sets in SO questions")
		}

		model, effort, route := false, false, false
		for id := range loadDecisionItems() {
			low := strings.ToLower(id)
			if strings.Contains(low, "effort") {
				effort = true
			}
			if strings.Contains(low, "model") && id != "reasoning.model_route" {
				model = true
			}
			if strings.Contains(low, "route") && id != "reasoning.model_route" {
				route = true
			}
		}
		if !effort {
			t.Error("BROKEN FA11 separation: no effort decision item")
		}
		if !model || !route {
			t.Errorf("PARTIAL FA11 separation: effort item present; standalone model=%v route=%v; model+route are one reasoning.model_route item",
				model, route)
		}
	})
}

func w30_mustJSON(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("SO POST is not JSON: %v", err)
	}
	return out
}

func TestSimFA12(t *testing.T) {
	t.Run("gate_key_binding", func(t *testing.T) {
		w30_env(t)
		cap := soCapability()
		const state = "classify gate target"
		base := w30_frame("initialize", 1, `,"state":"`+state+`"`)
		method := w30_frame("tools/call", 1, `,"state":"`+state+`"`)
		kBase := gateKeyBytes(base, cap)
		if kBase == gateKeyBytes(method, cap) {
			t.Error("BROKEN FA12 gateKey: method not bound (initialize and tools/call share a key)")
		}
		toolA := w30_frame("tools/call", 1, `,"state":"`+state+`","name":"read_file"`)
		toolB := w30_frame("tools/call", 1, `,"state":"`+state+`","name":"write_file"`)
		if gateKeyBytes(toolA, cap) == gateKeyBytes(toolB, cap) {
			t.Error("BROKEN FA12 gateKey: tool name not bound")
		}
		targetA := w30_frame("tools/call", 1, `,"state":"`+state+`","name":"read_file","arguments":{"path":"/sim/a"}`)
		targetB := w30_frame("tools/call", 1, `,"state":"`+state+`","name":"read_file","arguments":{"path":"/sim/b"}`)
		if gateKeyBytes(targetA, cap) == gateKeyBytes(targetB, cap) {
			t.Error("BROKEN FA12 gateKey: target not bound")
		}
		stateA := w30_frame("initialize", 1, `,"state":"classify alpha"`)
		stateB := w30_frame("initialize", 1, `,"state":"classify beta"`)
		if gateKeyBytes(stateA, cap) == gateKeyBytes(stateB, cap) {
			t.Error("BROKEN FA12 gateKey: full state hash not bound")
		}
		before := gateKeyBytes(base, cap)
		const promptName = "sim-fa-key-prompt"
		if err := addPrompt(promptName, "sim fa key binding body"); err != nil {
			t.Fatalf("addPrompt: %v", err)
		}
		t.Cleanup(func() { _ = removePrompt(promptName) })
		if gateKeyBytes(base, cap) == before {
			t.Error("BROKEN FA12 gateKey: active prompt-set hash not bound")
		}
	})

	t.Run("method_change_reuses_cache_cell", func(t *testing.T) {
		w30_env(t)
		p := &w30_probe{}
		p.install(t)
		br := newDecisionBridge()
		r1 := br.decideFrame(w30_itemFrame("initialize", 1, "reasoning.effort", "classify effort"))
		if r1.Decision != decPin {
			t.Fatalf("setup failed: %q %q", r1.Decision, r1.Reason)
		}
		// First admit primes the cache; id is stripped, so the next identical
		// frame must hit it without a second SO POST.
		r2 := br.decideFrame(w30_itemFrame("initialize", 2, "reasoning.effort", "classify effort"))
		if r2.Decision != decPin || r2.Reason != "cache-hit" || p.posts.Load() != 1 {
			t.Fatalf("setup failed: immediate cache-hit missing: %q %q posts=%d", r2.Decision, r2.Reason, p.posts.Load())
		}
		r3 := br.decideFrame(w30_itemFrame("tools/call", 3, "reasoning.effort", "classify effort"))
		if r3.Reason == "cache-hit" || p.posts.Load() != 2 {
			t.Errorf("BROKEN FA12 method binding in handler: method change served reason=%q posts=%d (want re-decision and 2 POSTs)",
				r3.Reason, p.posts.Load())
		}
	})

	t.Run("pin_expiry_not_bound", func(t *testing.T) {
		w30_env(t)
		p := &w30_probe{}
		p.install(t)
		br := newDecisionBridge()
		f := w30_itemFrame("tools/call", 1, "reasoning.effort", "classify effort alpha")
		r1 := br.decideFrame(f)
		if r1.Decision != decPin {
			t.Fatalf("setup failed: %q %q", r1.Decision, r1.Reason)
		}
		// The first admit primes the cache; the next identical frame hits it.
		rPrime := br.decideFrame(w30_itemFrame("tools/call", 2, "reasoning.effort", "classify effort alpha"))
		if rPrime.Decision != decPin || rPrime.Reason != "cache-hit" || p.posts.Load() != 1 {
			t.Fatalf("setup failed: immediate cache-hit missing: %q %q posts=%d", rPrime.Decision, rPrime.Reason, p.posts.Load())
		}
		br.pin.mu.Lock()
		br.pin.expires = time.Now().Add(-time.Second)
		br.pin.mu.Unlock()
		_ = br.pin.current()
		gen := br.pin.generation()
		r2 := br.decideFrame(f)
		if r2.Reason != "cache-hit" || p.posts.Load() != 1 {
			t.Errorf("BROKEN FA12 pin expiry: cache cell was bound to engine identity, not expiry (reason=%q posts=%d gen=%d)",
				r2.Reason, p.posts.Load(), gen)
		}
	})

	t.Run("receipt_nonce_and_chain", func(t *testing.T) {
		w30_env(t)
		p := &w30_probe{}
		p.install(t)
		br := newDecisionBridge()
		r1 := br.decideFrame(w30_itemFrame("initialize", 1, "reasoning.effort", "classify effort one"))
		r2 := br.decideFrame(w30_itemFrame("initialize", 2, "reasoning.effort", "classify effort two"))
		if r1.Decision != decPin || r2.Decision != decPin {
			t.Fatalf("setup failed: %q %q / %q %q", r1.Decision, r1.Reason, r2.Decision, r2.Reason)
		}
		data, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".walsy-system1", "receipt.chain"))
		if err != nil {
			t.Fatalf("read receipt chain: %v", err)
		}
		lines := w30_lines(string(data))
		if len(lines) != 2 {
			t.Fatalf("receipt lines=%d want 2: %q", len(lines), string(data))
		}
		var recs []map[string]any
		for _, line := range lines {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) != nil {
				t.Fatalf("receipt line is not JSON: %q", line)
			}
			recs = append(recs, rec)
		}
		n1, _ := recs[0]["nonce"].(string)
		n2, _ := recs[1]["nonce"].(string)
		b1, e1 := hex.DecodeString(n1)
		b2, e2 := hex.DecodeString(n2)
		if e1 != nil || e2 != nil || len(b1) != 16 || len(b2) != 16 {
			t.Errorf("BROKEN FA12 receipt nonce: %q / %q (want 32 hex chars each)", n1, n2)
		}
		if n1 == n2 {
			t.Errorf("BROKEN FA12 receipt nonce: duplicate nonce %q", n1)
		}
		if recs[0]["seq"] != float64(1) || recs[1]["seq"] != float64(2) {
			t.Errorf("BROKEN FA12 receipt chain: seq=%v/%v want 1/2", recs[0]["seq"], recs[1]["seq"])
		}
		if prev, _ := recs[0]["prev"].(string); prev != "" && prev != strings.Repeat("0", 64) {
			t.Errorf("BROKEN FA12 receipt chain: first prev=%q", recs[0]["prev"])
		}
		sum := chainHash(nil, []byte(lines[0]))
		if recs[1]["prev"] != hex.EncodeToString(sum[:]) {
			t.Errorf("BROKEN FA12 receipt chain: second prev=%q want %x", recs[1]["prev"], sum)
		}
	})

	t.Run("prompt_body_excluded_from_state", func(t *testing.T) {
		w30_env(t)
		const marker = "sim-fa-unique-prompt-marker-7f3a"
		if err := addPrompt("sim-fa-secret", marker); err != nil {
			t.Fatalf("addPrompt: %v", err)
		}
		t.Cleanup(func() { _ = removePrompt("sim-fa-secret") })
		p := &w30_probe{}
		p.install(t)
		br := newDecisionBridge()
		f := w30_frame("tools/call", 1, `,"item":"reasoning.effort","state":"classify effort","user_system_prompt":"`+marker+`"`)
		r := br.decideFrame(f)
		if r.Decision != decPin {
			t.Fatalf("setup failed: %q %q", r.Decision, r.Reason)
		}
		for i := 0; i < int(p.posts.Load()); i++ {
			if strings.Contains(p.body(i), marker) {
				t.Errorf("BROKEN FA12 prompt leak: marker found in SO POST %d", i)
			}
		}
		sum := sha256.Sum256([]byte(marker))
		if !strings.Contains(p.body(0), hex.EncodeToString(sum[:])) {
			t.Errorf("PARTIAL FA12 prompt refs: SHA-256 of prompt body absent from SO POST: %q", p.body(0))
		}
	})

	t.Run("endpoint_and_key_pin", func(t *testing.T) {
		w30_env(t)
		t.Setenv("WALSY_SO_MODE", "enforcing")
		t.Setenv("EVREN_API_KEY", "sim-pin-key")
		old := soEndpoint
		const pinned = "https://oracle-a.example/v1/systemone"
		soEndpoint = pinned
		t.Cleanup(func() { soEndpoint = old })
		br := newDecisionBridge()
		if len(br.oraclePin) != sha256.Size {
			t.Errorf("BROKEN FA12 oracle pin length=%d want %d", len(br.oraclePin), sha256.Size)
		}
		soEndpoint = "https://oracle-b.example/v1/systemone"
		resp, blocked := br.enforcingGate(w30_frame("initialize", 1, ""))
		if !blocked || !w30_hasDriftRefusal(t, resp) {
			t.Errorf("BROKEN FA12 endpoint drift not refused: blocked=%v resp=%q", blocked, resp)
		}
		soEndpoint = pinned
		t.Setenv("EVREN_API_KEY", "sim-pin-key-b")
		resp, blocked = br.enforcingGate(w30_frame("initialize", 2, ""))
		if !blocked || !w30_hasDriftRefusal(t, resp) {
			t.Errorf("BROKEN FA12 key drift not refused: blocked=%v resp=%q", blocked, resp)
		}
		t.Setenv("WALSY_SO_MODE", "advisory")
		soEndpoint = "https://oracle-b.example/v1/systemone"
		if _, blocked := br.enforcingGate(w30_frame("initialize", 3, "")); blocked {
			t.Error("BROKEN FA12 advisory mode blocked an oracle drift")
		}
	})

	t.Run("fail_closed_abstain", func(t *testing.T) {
		t.Run("malformed_reply", func(t *testing.T) {
			w30_env(t)
			p := &w30_probe{reply: func(map[string]any) string {
				return `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95,`
			}}
			p.install(t)
			br := newDecisionBridge()
			r := br.decideFrame(w30_itemFrame("initialize", 1, "reasoning.effort", "classify effort"))
			if r.Decision == decPin || !r.Abstained {
				t.Errorf("BROKEN FA12 fail-closed malformed: decision=%q abstained=%v", r.Decision, r.Abstained)
			}
		})
		t.Run("incomplete_probabilities", func(t *testing.T) {
			w30_env(t)
			p := &w30_probe{reply: func(req map[string]any) string {
				labels := w30_labels(req)
				winner := w30_winner(req, labels)
				b, _ := json.Marshal(map[string]any{"answers": map[string]any{"gate": map[string]any{
					"type": "choice", "choice": winner, "confidence": 0.95,
					"probabilities": map[string]float64{winner: 0.95},
				}}})
				return string(b)
			}}
			p.install(t)
			br := newDecisionBridge()
			r := br.decideFrame(w30_itemFrame("initialize", 1, "reasoning.effort", "classify effort"))
			if r.Decision == decPin || !r.Abstained {
				t.Errorf("BROKEN FA12 fail-closed incomplete probabilities: decision=%q abstained=%v probs=%v",
					r.Decision, r.Abstained, r.LastDecision.Probabilities)
			}
		})
		t.Run("out_of_range_probabilities", func(t *testing.T) {
			w30_env(t)
			p := &w30_probe{reply: func(req map[string]any) string {
				labels := w30_labels(req)
				winner := w30_winner(req, labels)
				other := "no"
				for _, l := range labels {
					if l != winner {
						other = l
						break
					}
				}
				b, _ := json.Marshal(map[string]any{"answers": map[string]any{"gate": map[string]any{
					"type": "choice", "choice": winner, "confidence": 0.95,
					"probabilities": map[string]float64{winner: 5, other: -4},
				}}})
				return string(b)
			}}
			p.install(t)
			br := newDecisionBridge()
			r := br.decideFrame(w30_itemFrame("initialize", 1, "reasoning.effort", "classify effort"))
			if r.Decision == decPin || !r.Abstained {
				t.Errorf("BROKEN FA12 fail-closed out-of-range probabilities: decision=%q abstained=%v probs=%v",
					r.Decision, r.Abstained, r.LastDecision.Probabilities)
			}
		})
	})
}

func w30_hasDriftRefusal(t *testing.T, resp []byte) bool {
	t.Helper()
	var reply gateReply
	if err := json.Unmarshal(resp, &reply); err != nil {
		return false
	}
	return reply.Error.Code == -32001 && strings.Contains(reply.Error.Message, "so-unpinned-oracle")
}
