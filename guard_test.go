package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

var _ net.Error = timeoutError{}

func TestClassifyTruthTable(t *testing.T) {
	cases := []struct {
		status int
		exc    error
		want   string
	}{
		{http.StatusRequestTimeout, nil, "transient"},
		{http.StatusTooManyRequests, nil, "transient"},
		{http.StatusInternalServerError, nil, "transient"},
		{http.StatusBadGateway, nil, "transient"},
		{http.StatusServiceUnavailable, nil, "transient"},
		{http.StatusGatewayTimeout, nil, "transient"},
		{http.StatusBadRequest, nil, "permanent"},
		{http.StatusUnauthorized, nil, "permanent"},
		{http.StatusForbidden, nil, "permanent"},
		{http.StatusNotFound, nil, "permanent"},
		{http.StatusUnprocessableEntity, nil, "permanent"},
		{http.StatusOK, nil, "ok"},
		{0, timeoutError{}, "transient"},
		{0, nil, "unknown"},
	}
	for _, c := range cases {
		if got := classify(c.status, c.exc); got != c.want {
			t.Fatalf("classify(%d, %v)=%q want %q", c.status, c.exc, got, c.want)
		}
	}
}

func TestIsDegraded(t *testing.T) {
	for _, c := range []struct {
		last string
		want bool
	}{
		{"transient", true},
		{"permanent", true},
		{"ok", false},
		{"unknown", false},
		{"", false},
	} {
		if got := isDegraded(c.last); got != c.want {
			t.Fatalf("isDegraded(%q)=%v want %v", c.last, got, c.want)
		}
	}
}

// A 503 is a reason-code, never an HTTP response the caller can emit.
func TestClassify503NeverReturnsStatus(t *testing.T) {
	got := classify(http.StatusServiceUnavailable, nil)
	if got != "transient" {
		t.Fatalf("503 must classify as transient reason-code, got %q", got)
	}
	rec := httptest.NewRecorder()
	_ = got // string only: no status written to the caller's response
	if rec.Code != http.StatusOK {
		t.Fatalf("caller response mutated: code=%d", rec.Code)
	}
}
