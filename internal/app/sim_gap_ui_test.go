package app

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// /api/keytest must turn the upstream status class into the named outcome the
// sheet shows. A probe that answers 200/401/403 is the credential verdict; any
// other answer is about the request or the service, and no answer at all is 0.
func TestSimGapUIKeytestClassifiesUpstreamStatus(t *testing.T) {
	const name = "SIM/PROBE"
	const secret = "sk-sim-gap-do-not-render"
	s, _, _ := simHome(t)

	cases := []struct {
		status int
		result string
	}{
		{http.StatusUnauthorized, "INVALID KEY"},
		{http.StatusForbidden, "FORBIDDEN"},
		{http.StatusTooManyRequests, "RATE LIMITED"},
		{http.StatusServiceUnavailable, "SERVER ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.result, func(t *testing.T) {
			var gotMethod, gotBody string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				buf := make([]byte, 4096)
				n, _ := r.Body.Read(buf)
				gotBody = string(buf[:n])
				if r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("x-api-key") != secret {
					t.Errorf("probe did not carry the stored key in both headers")
				}
				w.WriteHeader(tc.status)
			}))
			defer upstream.Close()

			if err := saveCustomProvider(name, upstream.URL+"/v1", "SIM_KEY"); err != nil {
				t.Fatal(err)
			}
			if err := saveModels(map[string][]string{name: {"sim-model"}}); err != nil {
				t.Fatal(err)
			}
			if err := saveKey(name, secret); err != nil {
				t.Fatal(err)
			}

			var out struct {
				Status int    `json:"status"`
				Result string `json:"result"`
				Winner string `json:"winner"`
			}
			code := postJSONTest(t, s.URL+"/api/keytest", map[string]string{"provider": name, "model": "sim-model"}, &out)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if out.Status != tc.status || out.Result != tc.result || out.Winner != "" {
				t.Fatalf("outcome = %+v, want %d/%q/no winner", out, tc.status, tc.result)
			}
			if gotMethod != http.MethodPost {
				t.Fatalf("probe method = %q, want POST", gotMethod)
			}
			if !strings.Contains(gotBody, `"questions"`) || strings.Contains(gotBody, `"messages"`) {
				t.Fatalf("probe was not the System One question body: %q", gotBody)
			}
		})
	}

	// A refused connection is no HTTP answer: status 0, unreachable, no winner.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr().String()
	dead.Close()
	if err := saveCustomProvider(name, "http://"+deadAddr+"/v1", "SIM_KEY"); err != nil {
		t.Fatal(err)
	}
	if err := saveModels(map[string][]string{name: {"sim-model"}}); err != nil {
		t.Fatal(err)
	}
	if err := saveKey(name, secret); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Status int    `json:"status"`
		Result string `json:"result"`
		Winner string `json:"winner"`
	}
	if code := postJSONTest(t, s.URL+"/api/keytest", map[string]string{"provider": name}, &out); code != http.StatusOK {
		t.Fatalf("unreachable probe status = %d, want 200", code)
	}
	if out.Status != 0 || out.Result != "ENDPOINT UNREACHABLE" || out.Winner != "" {
		t.Fatalf("unreachable probe = %+v, want 0/ENDPOINT UNREACHABLE/no winner", out)
	}
}

// Key add/remove/rotate over HTTP must hit the slot the form names, not just
// change the key count, and a removed slot must not shift the surviving values.
func TestSimGapUIKeySlotsOverHTTP(t *testing.T) {
	s, c, _ := simHome(t)
	const name = "SIM/SLOTS"

	if _, body := simPost(t, c, s, "/ui/providers", url.Values{
		"name": {name}, "base": {"http://127.0.0.1:1/v1"}, "keyEnv": {"SIM_KEY"},
	}); !strings.Contains(body, "PROVIDER SAVED") {
		t.Fatalf("add provider rendered %q, want PROVIDER SAVED", body)
	}
	keyForm := func(action, key, index string) url.Values {
		v := url.Values{"name": {name}, "action": {action}}
		if key != "" {
			v.Set("key", key)
		}
		if index != "" {
			v.Set("index", index)
		}
		return v
	}
	for _, key := range []string{"sim-key-1", "sim-key-2", "sim-key-3"} {
		if _, body := simPost(t, c, s, "/ui/providers/keys", keyForm("add", key, "")); !strings.Contains(body, "KEY ADDED") {
			t.Fatalf("add %q rendered %q, want KEY ADDED", key, body)
		}
	}

	// Slot 2 is the middle key: only it may go.
	if _, body := simPost(t, c, s, "/ui/providers/keys", keyForm("remove", "", "2")); !strings.Contains(body, "KEY REMOVED") {
		t.Fatalf("remove slot 2 rendered %q, want KEY REMOVED", body)
	}
	keys, err := loadKeys(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "sim-key-1" || keys[1] != "sim-key-3" {
		t.Fatalf("after slot 2 removal = %v, want [sim-key-1 sim-key-3]", keys)
	}

	// Rotate: append the new key, then drop the old first slot.
	if _, body := simPost(t, c, s, "/ui/providers/keys", keyForm("add", "sim-key-4", "")); !strings.Contains(body, "KEY ADDED") {
		t.Fatalf("rotate add rendered %q", body)
	}
	if _, body := simPost(t, c, s, "/ui/providers/keys", keyForm("remove", "", "1")); !strings.Contains(body, "KEY REMOVED") {
		t.Fatalf("rotate remove rendered %q", body)
	}
	keys, err = loadKeys(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "sim-key-3" || keys[1] != "sim-key-4" {
		t.Fatalf("after rotation = %v, want [sim-key-3 sim-key-4]", keys)
	}

	// A slot past the end is refused and leaves the list untouched.
	if _, body := simPost(t, c, s, "/ui/providers/keys", keyForm("remove", "", "9")); !strings.Contains(body, "INVALID KEY") {
		t.Fatalf("out-of-range remove rendered %q, want INVALID KEY", body)
	}
	if keys, _ := loadKeys(name); len(keys) != 2 {
		t.Fatalf("refused removal changed the key list: %v", keys)
	}
}

// The dashboard that rides along with MCP is loopback-only: a host-bearing bind
// spec is refused, and the listener is unreachable on a non-loopback address.
func TestSimGapUIDashboardBindsLoopbackOnly(t *testing.T) {
	port := freeLoopbackPort(t)
	startDashboardWithMCP(context.Background(), port)
	waitForDashboard(t, port)

	other := freeLoopbackPort(t)
	startDashboardWithMCP(context.Background(), "0.0.0.0:"+other)
	resp, err := http.Get("http://127.0.0.1:" + other + "/")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("non-loopback bind spec %q started a dashboard", "0.0.0.0:"+other)
	}

	if ip := nonLoopbackIPv4(); ip != "" {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, port), 300*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Fatalf("dashboard reachable on non-loopback %s:%s", ip, port)
		}
	}
}

// nonLoopbackIPv4 returns one local IPv4 address that is not loopback, or "".
func nonLoopbackIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ip, ok := a.(*net.IPNet)
		if !ok || ip.IP.IsLoopback() || ip.IP.To4() == nil || !ip.IP.IsGlobalUnicast() {
			continue
		}
		return ip.IP.String()
	}
	return ""
}
