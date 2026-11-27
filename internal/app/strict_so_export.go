package app

import (
	"encoding/json"
	"fmt"
)

// StrictSOObject decodes one size-capped SO JSON object with duplicate,
// case-variant, unknown-key and trailing-data rejection. Used by the release
// and final gates; allowlist nil accepts every exact-case top-level key.
func StrictSOObject(raw []byte, allowlist map[string]bool) (map[string]json.RawMessage, error) {
	fields, err := strictObject(raw, 1<<20, nil)
	if err != nil {
		return nil, err
	}
	if allowlist != nil {
		for k := range fields {
			if !allowlist[k] {
				return nil, fmt.Errorf("unknown key %q", k)
			}
		}
	}
	return fields, nil
}
