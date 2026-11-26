package app

import (
	"net/url"
	"testing"
)

// A name that owns no registry line, no models and no keys must be refused:
// reporting PROVIDER REMOVED for it claims a write that never happened.
func TestRemovalOfUnknownProviderIsRefused(t *testing.T) {
	s, c, _ := simHome(t)
	if err := saveCombo([]stateStep{{Provider: "Ghost Provider", Model: "g1"}}); err != nil {
		t.Fatalf("seed combo: %v", err)
	}
	resp, _ := simPost(t, c, s, "/ui/providers/remove", url.Values{"name": {"Ghost Provider"}})
	if msg := resp.Request.URL.Query().Get("msg"); msg != "INVALID PROVIDER" {
		t.Fatalf("unknown removal reported %q", msg)
	}
	if combo, _ := loadCombo(); len(combo) != 1 || combo[0].Provider != "Ghost Provider" {
		t.Fatalf("refused removal still mutated combo: %+v", combo)
	}
}
