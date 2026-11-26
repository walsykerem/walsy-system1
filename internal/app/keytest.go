package app

import (
	"encoding/json"
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

// soProbeModel is the System One model id used when a provider has no model
// of its own configured.
const soProbeModel = "clef"

// soProbeBody is the System One wire contract: a model id, a state string and
// a questions record. Every SO endpoint rejects a chat-shaped body (400/422),
// so a chat probe blames a key for a request that was never valid. The body
// carries no secret: the credential travels only in the headers.
func soProbeBody(model string) string {
	body, err := json.Marshal(map[string]any{
		"model": model,
		"state": "credential probe",
		"questions": map[string]soQuestion{
			"probe": {Type: "choice", Instructions: "Answer only to confirm the endpoint answered.",
				Criteria: map[string]string{
					"yes": "the endpoint answered", "no": "the endpoint did not answer",
				}},
		},
	})
	if err != nil {
		return ""
	}
	return string(body)
}

// providerModel returns the model id to probe: the requested one, else the
// provider's first configured model, else the System One default.
func providerModel(provider, requested string) string {
	if requested != "" {
		return requested
	}
	if models, err := loadModels(); err == nil {
		if list := models[provider]; len(list) > 0 {
			return list[0]
		}
	}
	return soProbeModel
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
	body := soProbeBody(providerModel(name, ""))
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
