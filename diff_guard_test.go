package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A destructive shell command is refused on a command tool even without a
// file body to diff. An ordinary write whose body merely mentions "rm" is not.
func TestArgsDestructiveCheck(t *testing.T) {
	if v := argsDestructiveCheck("run_command", json.RawMessage(`{"command":"rm -rf /tmp/x"}`)); !v.Refuse || v.Code == "" {
		t.Fatalf("rm -rf on a command tool must be refused, got %+v", v)
	}
	if v := argsDestructiveCheck("bash", json.RawMessage(`{"command":"dd if=/dev/zero of=/tmp/x"}`)); !v.Refuse {
		t.Fatal("dd of= must be refused on a command tool")
	}
	if v := argsDestructiveCheck("write_file", json.RawMessage(`{"path":"/tmp/x","content":"rm -rf /"}`)); v.Refuse {
		t.Fatal("a write whose content mentions rm must not be refused")
	}
	if v := argsDestructiveCheck("run_command", json.RawMessage(`{"command":"ls -la"}`)); v.Refuse {
		t.Fatal("a benign command must not be refused")
	}
}

func TestDiffSecurityCheck(t *testing.T) {
	longOld := ""
	for i := 0; i < diffDeleteLines+1; i++ {
		longOld += "old line " + string(rune('a'+i%26)) + "\n"
	}
	cases := []struct {
		name     string
		path     string
		old, new string
		code     string
	}{
		{"clean write", "a.go", "", "package main\n", ""},
		{"private key", "a.go", "", "key: -----BEGIN RSA PRIVATE KEY-----\n", "diff-secret"},
		{"openssh key", "a.go", "", "-----BEGIN OPENSSH PRIVATE KEY-----\n", "diff-secret"},
		{"aws key", "a.go", "", "aws = AKIAIOSFODNN7EXAMPLE\n", "diff-secret"},
		{"github token", "a.go", "", "t = ghp_0123456789abcdefghijklmnopqrstuvwxyz\n", "diff-secret"},
		{"slack token", "a.go", "", "t = xoxb-1234567890-abcdefghij\n", "diff-secret"},
		{"password literal", "a.go", "", "password = \"hunter2pass\"\n", "diff-secret"},
		{"json quoted key", "a.go", "", "{\"password\":\"hunter2pass\"}\n", "diff-secret"},
		{"escaped quoted key", "a.go", "", "{\\\"password\\\":\\\"hunter2pass\\\"}\n", "diff-secret"},
		{"secret containing skip word", "a.go", "", "password = \"hunter2$ecret\"\n", "diff-secret"},
		{"secret containing your", "a.go", "", "password = \"yoursTruly2024\"\n", "diff-secret"},
		{"api key literal", "a.go", "", "api_key: sk_live_9f8e7d6c5b4a\n", "diff-secret"},
		{"client_secret snake", "a.go", "", "client_secret = \"cs_9f8e7d6c5b4a3210\"\n", "diff-secret"},
		{"clientSecret camel", "a.go", "", "clientSecret = \"cs_9f8e7d6c5b4a3220\"\n", "diff-secret"},
		{"private key literal", "a.go", "", "private_key = \"pk_9f8e7d6c5b4a3230\"\n", "diff-secret"},
		{"bare token literal", "a.go", "", "token: abcdef1234567890\n", "diff-secret"},
		{"aws secret access key", "a.go", "", "AWS_SECRET_ACCESS_KEY = wJalrXUtnFEMI/K7MDENG/bPxRfiCYzQ9vB2\n", "diff-secret"},
		{"aws access key id", "a.go", "", "aws_access_key_id = AKIAIOSFODNN7EXAMPLE\n", "diff-secret"},
		{"15-char lowercase literal", "a.go", "", "password = \"abcdefghijklmno\"\n", "diff-secret"},
		{"jwt bearer token", "a.go", "", "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c\n", "diff-secret"},
		{"env ref passes", "a.go", "", "apiKey: os.Getenv(\"API_KEY\")\n", ""},
		{"templated passes", "a.go", "", "api_key = ${API_KEY}\n", ""},
		{"placeholder passes", "a.go", "", "password = \"changeme\"\n", ""},
		{"existing secret untouched", "a.go", "password = \"hunter2pass\"\n", "password = \"hunter2pass\"\nfunc x() {}\n", ""},
		{"wipe", "a.go", "package main\n", "\n", "diff-wipe"},
		{"pure deletion", "a.go", longOld, "", "diff-wipe"},
		{"pure deletion keeps one line", "a.go", longOld, "old line a\n", "diff-destructive"},
		{"small deletion", "a.go", "a\nb\nc\nd\ne\n", "a\nb\n", ""},
	}
	for _, c := range cases {
		got := diffSecurityCheck(c.path, c.old, c.new)
		if got.Code != c.code {
			t.Errorf("%s: code=%q want %q (verdict %+v)", c.name, got.Code, c.code, got)
		}
		if got.Refuse != (c.code != "") {
			t.Errorf("%s: refuse=%v with code %q", c.name, got.Refuse, got.Code)
		}
		if got.Refuse && got.Steer == "" {
			t.Errorf("%s: refusal without steer", c.name)
		}
		if c.code == "diff-secret" && strings.Contains(got.Steer, "hunter2pass") {
			t.Errorf("%s: steer leaks the secret", c.name)
		}
	}
}

func TestDiffLinesMultiset(t *testing.T) {
	added, removed := diffLines("a\nb\na\n", "a\nb\na\nc\n")
	if len(added) != 1 || added[0] != "c" || len(removed) != 0 {
		t.Fatalf("added=%v removed=%v", added, removed)
	}
	_, removed = diffLines("a\na\n", "a\n")
	if len(removed) != 1 || removed[0] != "a" {
		t.Fatalf("duplicate removal counted wrong: %v", removed)
	}
}

// oldContentOnDisk is what makes the wipe and pure-deletion arms reachable at
// all: with "" the write always looks additive. A missing path must stay "" and
// must not panic, since most writes target files that do not exist yet.
func TestOldContentOnDisk(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if got := oldContentOnDisk(p); got != "" {
		t.Fatalf("missing file = %q want empty", got)
	}
	if got := oldContentOnDisk(""); got != "" {
		t.Fatalf("empty path = %q want empty", got)
	}
	if err := os.WriteFile(p, []byte("keep\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := oldContentOnDisk(p); got != "keep\n" {
		t.Fatalf("existing file = %q", got)
	}
	if v := diffSecurityCheck(p, oldContentOnDisk(p), ""); v.Code != "diff-wipe" {
		t.Fatalf("emptying write on real file = %+v want diff-wipe", v)
	}
}

// A call with no file body (shell redirect, unknown schema) is scanned at the
// argument level, so the secret arm is not limited to the five content keys.
func TestArgsSecretCheck(t *testing.T) {
	secret := json.RawMessage(`{"command":"echo '-----BEGIN RSA PRIVATE KEY-----' > /tmp/k"}`)
	if v := argsSecretCheck(secret); v.Code != "diff-secret" || !v.Refuse {
		t.Fatalf("shell redirect = %+v want diff-secret", v)
	}
	clean := json.RawMessage(`{"command":"go test ./..."}`)
	if v := argsSecretCheck(clean); v.Refuse {
		t.Fatalf("clean command refused: %+v", v)
	}
}
