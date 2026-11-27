package app

import (
	"net/http"
	"testing"
	"time"
)

func TestFix5_10RetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name, header string
		want         int
	}{
		{"delay seconds", "2", 2},
		{"zero seconds", "0", 0},
		{"http date", "Fri, 27 Nov 2026 12:00:03 GMT", 3},
		{"past date", "Fri, 27 Nov 2026 11:59:59 GMT", -1},
		{"garbage", "not-a-date", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryAfterSeconds(tc.header, now); got != tc.want {
				t.Fatalf("retryAfterSeconds(%q) = %d, want %d", tc.header, got, tc.want)
			}
		})
	}
	if _, err := http.ParseTime("Fri, 27 Nov 2026 12:00:03 GMT"); err != nil {
		t.Fatalf("test fixture is not a valid HTTP date: %v", err)
	}
}
