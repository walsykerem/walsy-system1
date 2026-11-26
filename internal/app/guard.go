package app

import (
	"context"
	"errors"
	"net"
	"net/http"
)

// classify maps an SO failure to a reason-code string only. It never returns
// an HTTP status, so callers cannot turn a 503 into a 503 response.
func classify(status int, exc error) string {
	// A deadline is a distinct typed reason; other net.Error timeouts stay
	// transient so existing callers keep their coarse classification.
	if errors.Is(exc, context.DeadlineExceeded) {
		return "timeout"
	}
	if exc != nil {
		var ne net.Error
		if errors.As(exc, &ne) {
			return "transient"
		}
		return "unknown"
	}
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "transient"
	case http.StatusBadRequest, http.StatusUnauthorized,
		http.StatusForbidden, http.StatusNotFound,
		http.StatusUnprocessableEntity:
		return "permanent"
	case http.StatusOK:
		return "ok"
	default:
		return "unknown"
	}
}
