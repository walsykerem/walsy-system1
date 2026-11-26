package app

import (
	"bytes"
	"encoding/json"
	"errors"
)

// markerKey is the namespaced extension advertised under capabilities.experimental.
const markerKey = "com.kerem/systemone-route"

// markerValue is SDK default-off: clients must opt in explicitly.
const markerValue = `{"enabled":false,"mode":"relay"}`

type frame struct {
	Raw    []byte
	ID     json.RawMessage
	Method string
	Result json.RawMessage
	Params json.RawMessage
}

// decodeFrame parses a JSON-RPC 2.0 frame. Raw is kept verbatim; unknown methods pass.
func decodeFrame(raw []byte) (frame, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] != '{' {
		return frame{}, errors.New("not a JSON-RPC object")
	}
	var env struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Params json.RawMessage `json:"params"`
	}
	// Inbound frames share the transport's 8 MiB budget; the 1 MiB cap is for SO replies only.
	if _, err := strictObject(t, maxHTTPFrame, nil); err != nil {
		return frame{}, err
	}
	if err := json.Unmarshal(t, &env); err != nil {
		return frame{}, err
	}
	return frame{Raw: raw, ID: env.ID, Method: env.Method, Result: env.Result, Params: env.Params}, nil
}

// isInitializeResult reports a response to initialize: no method, capabilities + protocolVersion.
func isInitializeResult(f frame) bool {
	if f.Method != "" || len(f.Result) == 0 {
		return false
	}
	var r map[string]json.RawMessage
	if err := json.Unmarshal(f.Result, &r); err != nil {
		return false
	}
	_, hasPV := r["protocolVersion"]
	_, hasCaps := r["capabilities"]
	return hasPV && hasCaps
}

// addRouteCapability adds the opt-in marker under capabilities.experimental.
// Only the capabilities value span is rewritten; id/_meta.progressToken/
// protocolVersion and unknown capabilities keep their original bytes.
// Malformed or unexpected JSON is returned unchanged (safe passthrough).
func addRouteCapability(result []byte) []byte {
	t := bytes.TrimSpace(result)
	if len(t) == 0 || t[0] != '{' {
		return result
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(t, &top); err != nil {
		return result
	}
	if _, ok := top["protocolVersion"]; !ok {
		return result
	}
	capRaw, ok := top["capabilities"]
	if !ok {
		return result
	}
	caps := []byte(capRaw)
	if len(caps) == 0 || caps[0] != '{' {
		return result
	}

	var newCaps []byte
	if expRaw, exists := getMemberValue(caps, "experimental"); exists {
		exp := []byte(expRaw)
		if len(exp) == 0 || exp[0] != '{' {
			return result
		}
		if _, has := getMemberValue(exp, markerKey); has {
			return result // already advertised; idempotent
		}
		newExp, ok := appendMember(exp, markerKey, []byte(markerValue))
		if !ok {
			return result
		}
		newCaps, ok = replaceMemberValue(caps, "experimental", newExp)
		if !ok {
			return result
		}
	} else {
		entry := []byte(`{"` + markerKey + `":` + markerValue + `}`)
		var ok bool
		newCaps, ok = appendMember(caps, "experimental", entry)
		if !ok {
			return result
		}
	}

	out, ok := replaceMemberValue(t, "capabilities", newCaps)
	if !ok {
		return result
	}
	return out
}

// replaceMemberValue swaps one object member's value span, preserving all other bytes.
func replaceMemberValue(obj []byte, key string, value []byte) ([]byte, bool) {
	vs, ve, found, ok := memberValueSpan(obj, key)
	if !ok || !found {
		return nil, false
	}
	out := make([]byte, 0, len(obj)-(ve-vs)+len(value))
	out = append(out, obj[:vs]...)
	out = append(out, value...)
	out = append(out, obj[ve:]...)
	return out, true
}

// getMemberValue returns the raw value bytes for key.
func getMemberValue(obj []byte, key string) ([]byte, bool) {
	vs, ve, found, ok := memberValueSpan(obj, key)
	if !ok || !found {
		return nil, false
	}
	return obj[vs:ve], true
}

// memberValueSpan locates a member's value span; ok=false means malformed object.
func memberValueSpan(obj []byte, key string) (start, end int, found, ok bool) {
	if len(obj) == 0 || obj[0] != '{' {
		return 0, 0, false, false
	}
	i := skipWS(obj, 1)
	for i < len(obj) && obj[i] != '}' {
		ks, ke, err := scanString(obj, i)
		if err != nil {
			return 0, 0, false, false
		}
		i = skipWS(obj, ke)
		if i >= len(obj) || obj[i] != ':' {
			return 0, 0, false, false
		}
		i = skipWS(obj, i+1)
		vs := i
		ve, err := scanValue(obj, vs)
		if err != nil {
			return 0, 0, false, false
		}
		var k string
		if json.Unmarshal(obj[ks:ke], &k) != nil {
			return 0, 0, false, false
		}
		if k == key {
			return vs, ve, true, true
		}
		i = skipWS(obj, ve)
		if i < len(obj) && obj[i] == ',' {
			i = skipWS(obj, i+1)
			continue
		}
		return 0, 0, false, false
	}
	return 0, 0, false, true
}

// appendMember inserts a new member before the object's closing brace.
func appendMember(obj []byte, key string, value []byte) ([]byte, bool) {
	end := objectEnd(obj)
	if end < 0 {
		return nil, false
	}
	entry, err := json.Marshal(key)
	if err != nil {
		return nil, false
	}
	entry = append(entry, ':')
	entry = append(entry, value...)
	comma := []byte{}
	if skipWS(obj, 1) < end {
		comma = []byte(",")
	}
	out := make([]byte, 0, len(obj)+len(entry)+len(comma))
	out = append(out, obj[:end]...)
	out = append(out, comma...)
	out = append(out, entry...)
	out = append(out, obj[end:]...)
	return out, true
}

func objectEnd(b []byte) int {
	depth, inStr := 0, false
	for j := 0; j < len(b); j++ {
		c := b[j]
		if inStr {
			if c == '\\' {
				j++
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			continue
		}
		if c == '{' {
			depth++
		}
		if c == '}' {
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

func skipWS(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

func scanString(b []byte, i int) (int, int, error) {
	if i >= len(b) || b[i] != '"' {
		return 0, 0, errors.New("expected string")
	}
	j := i + 1
	for j < len(b) {
		switch b[j] {
		case '\\':
			j += 2
		case '"':
			return i, j + 1, nil
		default:
			j++
		}
	}
	return 0, 0, errors.New("unterminated string")
}

func scanValue(b []byte, i int) (int, error) {
	i = skipWS(b, i)
	if i >= len(b) {
		return 0, errors.New("unexpected end")
	}
	switch b[i] {
	case '"':
		_, e, err := scanString(b, i)
		return e, err
	case '{', '[':
		depth, inStr := 0, false
		for j := i; j < len(b); j++ {
			c := b[j]
			if inStr {
				if c == '\\' {
					j++
					continue
				}
				if c == '"' {
					inStr = false
				}
				continue
			}
			if c == '"' {
				inStr = true
				continue
			}
			if c == '{' || c == '[' {
				depth++
			}
			if c == '}' || c == ']' {
				depth--
				if depth == 0 {
					return j + 1, nil
				}
			}
		}
		return 0, errors.New("unbalanced value")
	default:
		j := i
		for j < len(b) && b[j] != ',' && b[j] != '}' && b[j] != ']' && b[j] != ' ' && b[j] != '\t' && b[j] != '\n' && b[j] != '\r' {
			j++
		}
		return j, nil
	}
}
