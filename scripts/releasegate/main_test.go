package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerdict(t *testing.T) {
	cases := []struct {
		name    string
		a       soAnswer
		wantErr bool
	}{
		{"complete high", soAnswer{Type: "choice", Choice: "complete", Confidence: 0.95}, false},
		{"missing type", soAnswer{Choice: "complete", Confidence: 0.95}, true},
		{"wrong type", soAnswer{Type: "score", Choice: "complete", Confidence: 0.95}, true},
		{"incomplete high", soAnswer{Type: "choice", Choice: "incomplete", Confidence: 0.95}, true},
		{"complete low", soAnswer{Type: "choice", Choice: "complete", Confidence: 0.5}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verdict(tc.a, 0.8)
			if got := err != nil; got != tc.wantErr {
				t.Fatalf("verdict() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestFinalStateClaimsOnlyLocalVerification(t *testing.T) {
	for _, want := range []string{"PENDING", "NOT a release decision", "recorded outputs, NOT re-run"} {
		if !strings.Contains(finalStateFmt, want) {
			t.Fatalf("finalStateFmt omits %q; do not restore an evidence-free release claim", want)
		}
	}
}

func TestDecodeResponseWithRoutedModel(t *testing.T) {
	const raw = `{"model":"clef","routed_model":"Cloudflare/clef","answers":{"final":{"type":"choice","choice":"complete","confidence":0.97}},"usage":{"total_tokens":10}}`
	out, err := decodeResponse(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if out.RoutedModel != "Cloudflare/clef" {
		t.Fatalf("routed_model = %q", out.RoutedModel)
	}
	if out.Answers["final"].Choice != "complete" {
		t.Fatalf("choice = %q", out.Answers["final"].Choice)
	}
}

func TestCheckModel(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  soResponse
		pass bool
	}{
		{"empty", soResponse{}, true},
		{"clef", soResponse{Model: "clef"}, true},
		{"cloudflare", soResponse{RoutedModel: "Cloudflare/clef"}, true},
		{"bad model", soResponse{Model: "gpt-4"}, false},
		{"bad routed", soResponse{Model: "clef", RoutedModel: "other/model"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkModel(tc.out)
			if (err == nil) != tc.pass {
				t.Fatalf("checkModel(%+v) = %v, want pass %t", tc.out, err, tc.pass)
			}
		})
	}
}

func writeConfig(t *testing.T, home, owner string) {
	t.Helper()
	dir := filepath.Join(home, ".walsy-system1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.txt"), []byte("m1_approval_owner="+owner+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalOwnerBinding(t *testing.T) {
	for _, tc := range []struct {
		name, env, cfg, want string
	}{
		{"both same", "alice", "alice", "alice"},
		{"env only", "alice", "", ""},
		{"config only", "", "alice", ""},
		{"mismatch", "alice", "bob", ""},
		{"empty", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			writeConfig(t, home, tc.cfg)
			t.Setenv("RELEASE_APPROVAL", tc.env)
			got, err := approvalOwner()
			if got != tc.want || (err == nil) != (tc.want != "") {
				t.Fatalf("approvalOwner() = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestLocalEvidenceAndRunState(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "release.tag"), []byte("0.1.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "CHANGELOG.md"), []byte("# 0.1.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "scripts", "finalgate", "evidence")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, rel := range evidenceFiles {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte("== 1/10 gofmt ==\nrelease gate: PASS\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := localEvidence(root)
	if err != nil || !strings.Contains(got, "== 1/10 gofmt ==") {
		t.Fatalf("localEvidence omitted recorded evidence: %v", err)
	}
	if _, err := localEvidence(t.TempDir()); err == nil {
		t.Fatal("localEvidence accepted a tree with no evidence files")
	}
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(evidenceFiles[0]))); err != nil {
		t.Fatal(err)
	}
	if _, err := localEvidence(root); err == nil {
		t.Fatal("localEvidence accepted a tree with a missing evidence file")
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(evidenceFiles[0])), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, home, "alice")
	t.Setenv("RELEASE_APPROVAL", "alice")
	t.Setenv("EVREN_API_KEY", "dummy-key-not-a-secret")
	var gotState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(b, &body)
		gotState = body.State
		_, _ = io.WriteString(w, `{"model":"clef","routed_model":"Cloudflare/clef","answers":{"final":{"type":"choice","choice":"complete","confidence":0.9}}}`)
	}))
	defer srv.Close()
	t.Setenv("WALSY_SO_ENDPOINT", srv.URL)
	t.Chdir(root)
	if err := run(0.8); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"0.1.0", "scripts/finalgate/evidence/K12.txt", "== 1/10 gofmt ==", "M1 approval owner: alice"} {
		if !strings.Contains(gotState, want) {
			t.Fatalf("state %q omits %q", gotState, want)
		}
	}
	for _, stale := range []string{"70/70", "MANIFEST", "race suite green", "ascii clean", "recon digest current", "stages 1-8 green"} {
		if strings.Contains(gotState, stale) {
			t.Fatalf("state %q still claims unverified %q", gotState, stale)
		}
	}
}

func TestRunRejectsApprovalBypass(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "release.tag"), []byte("0.1.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "CHANGELOG.md"), []byte("# 0.1.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("EVREN_API_KEY", "dummy-key-not-a-secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"model":"clef","answers":{"final":{"type":"choice","choice":"complete","confidence":0.9}}}`)
	}))
	defer srv.Close()
	t.Setenv("WALSY_SO_ENDPOINT", srv.URL)
	for _, tc := range []struct {
		name, env, cfg, want string
	}{
		{"env only bypass", "alice", "", "approval requires both"},
		{"config only bypass", "", "alice", "approval requires both"},
		{"mismatch", "alice", "bob", "approval mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			writeConfig(t, home, tc.cfg)
			t.Setenv("RELEASE_APPROVAL", tc.env)
			if err := run(0.8); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRunRejectsBadVerdictSchemaAndModel(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "release.tag"), []byte("0.1.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "CHANGELOG.md"), []byte("# 0.1.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("EVREN_API_KEY", "dummy-key-not-a-secret")
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, home, "alice")
	t.Setenv("RELEASE_APPROVAL", "alice")
	for _, tc := range []struct {
		name, reply, want string
	}{
		{"missing type", `{"answers":{"final":{"choice":"complete","confidence":0.9}}}`, "not choice"},
		{"wrong type", `{"answers":{"final":{"type":"score","choice":"complete","confidence":0.9}}}`, "not choice"},
		{"wrong model", `{"model":"gpt-4","answers":{"final":{"type":"choice","choice":"complete","confidence":0.9}}}`, "not clef"},
		{"correct", `{"model":"clef","routed_model":"Cloudflare/clef","answers":{"final":{"type":"choice","choice":"complete","confidence":0.9}}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tc.reply)
			}))
			defer srv.Close()
			t.Setenv("WALSY_SO_ENDPOINT", srv.URL)
			err := run(0.8)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run error = %v, want %q", err, tc.want)
			}
		})
	}
}
