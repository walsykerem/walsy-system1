package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// strictObject decodes a JSON object at a trust boundary: size-capped, exactly
// one document, no duplicate or case-variant keys at any depth, and an optional
// top-level allowlist. Unknown top-level keys are rejected rather than silently
// last-wins, so a new endpoint field cannot slip through the parser unnoticed.
// allowlist nil means allow every key (still duplicate-checked).
func strictObject(raw []byte, maxBytes int, allowlist map[string]bool) (map[string]json.RawMessage, error) {
	if maxBytes > 0 && len(raw) > maxBytes {
		return nil, errors.New("too large")
	}
	out, err := strictFields(raw, allowlist)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// strictFields parses one object and validates every nested value.
func strictFields(raw []byte, allowlist map[string]bool) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("not an object")
	}
	out := make(map[string]json.RawMessage)
	seen := make(map[string]bool)
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, errors.New("bad key")
		}
		fold := strings.ToLower(key)
		if seen[fold] {
			return nil, errors.New("duplicate key")
		}
		seen[fold] = true
		if allowlist != nil && !allowlist[key] {
			return nil, errors.New("unknown key")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if err := strictValue(v); err != nil {
			return nil, err
		}
		out[key] = v
	}
	if tok, err = dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, errors.New("unterminated object")
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return out, nil
}

// strictValue validates nested objects/arrays for duplicate keys. Scalars are
// already validated by encoding/json's decoder.
func strictValue(raw []byte) error {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return errors.New("empty value")
	}
	switch t[0] {
	case '{':
		_, err := strictFields(t, nil)
		return err
	case '[':
		dec := json.NewDecoder(bytes.NewReader(t))
		if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
			return errors.New("bad array")
		}
		for dec.More() {
			var v json.RawMessage
			if err := dec.Decode(&v); err != nil {
				return err
			}
			if err := strictValue(v); err != nil {
				return err
			}
		}
		if tok, err := dec.Token(); err != nil || tok != json.Delim(']') {
			return errors.New("unterminated array")
		}
		if _, err := dec.Token(); err != io.EOF {
			return errors.New("trailing data")
		}
	}
	return nil
}
