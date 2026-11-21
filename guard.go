package main

import (
	"context"
	"errors"
	"net"
	"net/http"
)

// classify maps an SO failure to a reason-code string only. It never returns
// an HTTP status, so callers cannot turn a 503 into a 503 response.
func classify(status int, exc error) string {
	if exc != nil {
		var ne net.Error
		if errors.As(exc, &ne) || errors.Is(exc, context.DeadlineExceeded) {
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

// isDegraded is the single bool: any classified failure means passthrough,
// no retry, no breaker.
func isDegraded(last string) bool {
	return last == "transient" || last == "permanent"
}
