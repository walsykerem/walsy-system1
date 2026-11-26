package main

import (
	"strings"
	"testing"
)

func TestClassifyFailure(t *testing.T) {
	cases := []struct{ text, bucket string }{
		{"go test ./...\npanic: test timed out after 10m0s", failTimeout},
		{"dial tcp: lookup proxy.local: no such host", failEnv},
		{"go: downloading example.com/x v0.1.0", failEnv},
		{"warning: flaky test retried and passed", failFlake},
		{"--- FAIL: TestRoute (0.02s)\n    route_test.go:12: got 1 want 2", failTest},
		{"FAIL\twalsy-system1\t0.4s", failTest},
		{"cannot use x (variable of type int) as string value in argument", failType},
		{"*T does not implement io.Reader (missing method Read)", failType},
		{"x_test.go:12: undefined: foo", failCompile},
		{"syntax error: unexpected }, expecting )", failCompile},
		{"", failCompile},
	}
	for _, c := range cases {
		if got := classifyFailure(c.text); got != c.bucket {
			t.Errorf("classifyFailure(%q)=%q want %q", c.text, got, c.bucket)
		}
	}
}

func TestFailureSignatureStable(t *testing.T) {
	a := "--- FAIL: TestX (0.02s)\n    x_test.go:12: expected 1 got 2\n    /tmp/build-1234/log: noise"
	b := "--- FAIL: TestX (1.23s)\n    x_test.go:99: expected 1 got 2\n    /tmp/build-9999/log: noise"
	if sa, sb := failureSignature(a), failureSignature(b); sa != sb {
		t.Fatalf("same failure got different signatures: %q vs %q", sa, sb)
	}
	c := "--- FAIL: TestX (0.02s)\n    x_test.go:12: expected 3 got 4"
	if failureSignature(a) == failureSignature(c) {
		t.Fatal("different failures share a signature")
	}
	sig := failureSignature(a)
	if !strings.HasPrefix(sig, failTest+":") {
		t.Fatalf("signature lost its bucket: %q", sig)
	}
}

func TestFailureSignatureFeedsAttemptGuard(t *testing.T) {
	out := "--- FAIL: TestY (0.01s)\n    y_test.go:3: boom"
	g := newAttemptGuard()
	var sig string
	for i := 1; i <= attemptRevertAt; i++ {
		sig = failureSignature(out)
		revert, _ := g.fail(sig)
		if revert != (i == attemptRevertAt) {
			t.Fatalf("attempt %d: revert=%v", i, revert)
		}
	}
}
