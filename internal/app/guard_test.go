package app

import (
	"net"
	"net/http"
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
