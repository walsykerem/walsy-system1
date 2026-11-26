package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// gapSmallHome points HOME at a fresh temp dir so config.txt reads are hermetic.
func gapSmallHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestGapSmallPromotionOK(t *testing.T) {
	home := gapSmallHome(t)
	t.Setenv("WALSY_SO_MODE", soModeAdvisory)
	t.Setenv("WALSY_SO_APPROVER", "")

	if promotionOK() {
		t.Fatal("promotionOK() = true with no M1 owner / confidence, want fail-closed false")
	}

	dir := filepath.Join(home, ".walsy-system1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	body := "so_mode=advisory\nm1_approval_owner=gap-small\nso_go_confidence=0.9\n"
	if err := os.WriteFile(filepath.Join(dir, "config.txt"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if !promotionOK() {
		t.Fatal("promotionOK() = false with owner and confidence 0.9, want true")
	}
}

func TestGapSmallServeHTTP(t *testing.T) {
	gapSmallHome(t)
	t.Setenv("WALSY_SO_MODE", soModeAdvisory)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- serveHTTP(l) }()

	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Post(
		"http://"+l.Addr().String(), "application/json", bytes.NewReader(frame))
	if err != nil {
		_ = l.Close()
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = l.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		_ = l.Close()
		t.Fatalf("response is not JSON-RPC: %v (%q)", err, body)
	}
	if out["jsonrpc"] != "2.0" || out["id"] != float64(1) {
		_ = l.Close()
		t.Fatalf("response shape = %v, want jsonrpc 2.0 id 1", out)
	}
	if _, ok := out["result"]; !ok {
		if _, ok := out["error"]; !ok && out["method"] != "tools/list" {
			_ = l.Close()
			t.Fatalf("response has neither result, error, nor echoed method: %v", out)
		}
	}

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("serveHTTP did not return after listener close")
	}
}
