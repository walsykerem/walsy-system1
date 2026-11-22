package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// After an in-sheet write only the iframe navigates, so the home document must
// be told to reload; an empty sheet must stay silent or the parent loops.
func TestUISheetRefreshesParentOnMessage(t *testing.T) {
	const originGuard = `window.location.origin`
	cases := []struct {
		label string
		path  string
		want  bool
	}{
		{"provider sheet with msg", "/ui/providers/sheet?name=Evren+System+One&msg=MODEL+ADDED", true},
		{"provider sheet without msg", "/ui/providers/sheet?name=Evren+System+One", false},
		{"combo add sheet with msg", "/ui/combo/add/sheet?msg=MODEL+ADDED", true},
		{"combo add sheet without msg", "/ui/combo/add/sheet", false},
		{"provider sheet refusal", "/ui/providers/sheet?name=Evren+System+One&msg=ENDPOINT+REJECTED%3A+NEEDS+A+FULL+URL", false},
		{"provider sheet key test", "/ui/providers/sheet?name=Evren+System+One&msg=KEY+TEST+401+UNAUTHORIZED", false},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			rec := httptest.NewRecorder()
			NewDashboardHandler().ServeHTTP(rec, dashRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			body := rec.Body.String()
			hasPost := strings.Contains(body, `postMessage('walsy-refresh|`)
			if hasPost != tc.want {
				t.Fatalf("postMessage present = %v, want %v", hasPost, tc.want)
			}
			if tc.want && !strings.Contains(body, originGuard) {
				t.Fatal("refresh post missing its origin guard")
			}
		})
	}
}

func TestUIHomeListensForSheetRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rec := httptest.NewRecorder()
	NewDashboardHandler().ServeHTTP(rec, dashRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `addEventListener('message'`) {
		t.Fatal("home document missing the sheet-refresh listener")
	}
	// The reload keeps the sheet hash and carries the sheet's message, so the
	// success toast renders on the home page instead of dying with the iframe.
	if !strings.Contains(body, "window.location.hash") || !strings.Contains(body, "window.location.href") {
		t.Fatal("home refresh drops the sheet hash or the sheet message")
	}
	if strings.Contains(body, `postMessage('walsy-refresh|`) {
		t.Fatal("home document must not post a refresh to itself")
	}
}

// A sheet iframe is fetched once per parent document, so reopening a sheet
// would show the state from before the last write. The home document must
// re-src the open sheet frame with a nonce, and must keep the frame's own
// query (?name=Provider) by appending with & instead of rebuilding the URL.
func TestUIHomeReloadsOpenSheetFrame(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rec := httptest.NewRecorder()
	NewDashboardHandler().ServeHTTP(rec, dashRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"addEventListener('hashchange'",
		`querySelector('iframe.sheet-frame')`,
		"frame.getAttribute('src'), location.href)",
		"searchParams.set('t', String(Date.now()))",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("home document missing iframe reload fragment %q", want)
		}
	}
}

// Every sheet iframe is rendered once with the parent document. Reopening a
// sheet must re-fetch it (nonce in the query, sheet address intact) or the
// panel shows the state from before the last write.
func TestUIHomeReloadsSheetIframeOnOpen(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rec := httptest.NewRecorder()
	NewDashboardHandler().ServeHTTP(rec, dashRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"addEventListener('hashchange'",
		"document.querySelector(':target')",
		"querySelector('iframe.sheet-frame')",
		"new URL(frame.getAttribute('src'), location.href)",
		"searchParams.set('t', String(Date.now()))",
		"window.location.reload();",
		"sessionStorage.setItem('walsy-y'",
		"opener.focus({ preventScroll: true });",
		"JSON.stringify(location.hash)",
		"addEventListener('pageshow'",
		"close.focus({ preventScroll: true });",
		"if (e.persisted) location.reload();",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("home document missing the sheet-iframe reload (%q)", want)
		}
	}
}
