package main

import (
	"bytes"
	"container/list"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

const (
	cacheCapacity = 1024
)

// --- canonical state (byte-stable) ---------------------------------------

// volatileKeys are dropped recursively: they change every call without
// changing the decision. Compared in a normalized (case/dash/underscore-free) form.
var volatileKeys = map[string]bool{
	"timestamp": true, "time": true, "ts": true, "date": true,
	"messageref": true, "msgref": true, "messageid": true, "msgid": true,
	"tooloutput": true, "toolresult": true, "toolcallid": true,
	"client": true, "clientname": true, "clientid": true,
	"session": true, "sessionid": true, "requestid": true,
}

func normKey(k string) string {
	var b strings.Builder
	for _, r := range k {
		if r == '-' || r == '_' || r == ' ' {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func stripVolatile(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, stripVolatile(e))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if volatileKeys[normKey(k)] {
				continue
			}
			out[k] = stripVolatile(val)
		}
		return out
	default:
		return v
	}
}

// encString goes through Encoder (not json.Marshal) so SetEscapeHTML(false) holds.
func encString(s string) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

func canonNumber(raw string) string {
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return "n:" + raw
	}
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Inf"
	}
	if math.IsInf(f, -1) {
		return "-Inf"
	}
	if f == 0 {
		f = 0 // fold -0
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func encodeCanon(v any, b *strings.Builder) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		b.WriteString(canonNumber(string(x)))
	case float64:
		b.WriteString(canonNumber(strconv.FormatFloat(x, 'g', -1, 64)))
	case string:
		b.WriteString(encString(x))
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			encodeCanon(e, b)
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // sort AFTER canonicalize/strip
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(encString(k))
			b.WriteByte(':')
			encodeCanon(x[k], b)
		}
		b.WriteByte('}')
	default:
		b.WriteString(encString(fmt.Sprint(v)))
	}
}

// foldAlias is the L2 unicode.SimpleFold representative (stdlib has no NFC/NFD).
func foldAlias(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		min := r
		for n := unicode.SimpleFold(r); n != r; n = unicode.SimpleFold(n) {
			if n < min {
				min = n
			}
		}
		b.WriteRune(min)
	}
	return b.String()
}

// canonicalState returns a deterministic string. Callers hash it into a key;
// gateKey in decision_bridge.go additionally hashes request bytes and the
// capability config. No map[string]any marshal.
func canonicalState(s string) string {
	var v any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		c := "raw:" + encString(s)
		return c + "\x00" + foldAlias(c)
	}
	var b strings.Builder
	encodeCanon(stripVolatile(v), &b)
	c := "v1:" + b.String()
	return c + "\x00" + foldAlias(c)
}

// --- decision cache -------------------------------------------------------

// cachedDecision stores only the typed decision + coarse bucket. Raw conf is
// never held.
type cachedDecision struct {
	Choice     string
	ConfBucket string
	Reason     string
}

type cacheCell struct {
	key string
	dec cachedDecision
}

type decisionCache struct {
	mu  sync.Mutex
	cap int
	ll  *list.List
	idx map[string]*list.Element
}

func newDecisionCache() *decisionCache {
	return &decisionCache{cap: cacheCapacity, ll: list.New(), idx: make(map[string]*list.Element)}
}

func (c *decisionCache) lookup(key string) (cachedDecision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*cacheCell).dec, true
	}
	return cachedDecision{}, false
}

// store writes the cell under the mutex.
func (c *decisionCache) store(key string, d cachedDecision) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, d)
}

// storeLocked mutates idx/ll; the caller must hold c.mu.
func (c *decisionCache) storeLocked(key string, d cachedDecision) {
	if el, ok := c.idx[key]; ok {
		el.Value.(*cacheCell).dec = d
		c.ll.MoveToFront(el)
		return
	}
	c.idx[key] = c.ll.PushFront(&cacheCell{key: key, dec: d})
	for c.ll.Len() > c.cap {
		old := c.ll.Back()
		if old == nil {
			return
		}
		delete(c.idx, old.Value.(*cacheCell).key)
		c.ll.Remove(old)
	}
}
