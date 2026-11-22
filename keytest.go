package main

import (
	"io"
	"net/http"
	"strings"
	"time"
)

const keyTestTimeout = 2 * time.Second

var keyTestClient = &http.Client{
	Timeout: keyTestTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// testProviderKey probes the configured base with one stored key. It reports
// only a coarse outcome: the key value and raw base are never echoed.
func testProviderKey(name string, index int) string {
	keys, err := loadKeys(name)
	if err != nil || index < 1 || index > len(keys) {
		return "INVALID KEY"
	}
	view, err := providerView()
	if err != nil {
		return "STATE UNAVAILABLE"
	}
	base := providerBase(view, name)
	if !validProviderBase(base) {
		return "ENDPOINT INVALID"
	}
	// POST, not GET: gateways such as a System One endpoint only answer POST, so a
	// GET probe could never distinguish a good key from a missing route.
	body := `{"messages":[{"role":"user","content":"ping"}]}`
	req, err := http.NewRequest(http.MethodPost, base, strings.NewReader(body))
	if err != nil {
		return "ENDPOINT INVALID"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+keys[index-1])
	req.Header.Set("x-api-key", keys[index-1])
	resp, err := keyTestClient.Do(req)
	if err != nil {
		return "ENDPOINT UNREACHABLE"
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return "KEY OK"
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "KEY REJECTED"
	default:
		return "ENDPOINT RESPONDED"
	}
}
