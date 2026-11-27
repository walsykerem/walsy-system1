package app

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A14: 50 concurrent SO + dashboard + stdio paths must not read the mutable
// SO globals while a config rotation holds the write lock. No external service:
// both judges are loopback httptest servers. The first request blocks so the
// test can prove with TryLock/Lock (and atomic state under -race) that the
// decision goroutines hold the read lock before rotation is allowed.
func TestFix421ConcurrentSOConfigRotation(t *testing.T) {
	const answer = `{"answers":{"q":{"choice":"no","confidence":0.2}},"usage":{}}`
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()

	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Only the stdio/decision path carries this marker: the handshake must
		// prove that path holds the read lock, not the test's own wrappers.
		if bytes.Contains(body, []byte("FIX421UNIQUE")) {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		<-release
		_, _ = w.Write([]byte(answer))
	}))
	defer blocking.Close()
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(answer))
	}))
	defer fast.Close()

	oldEP, oldClient := soEndpoint, soClient
	soConfigMu.Lock()
	soEndpoint, soClient = blocking.URL, blocking.Client()
	soConfigMu.Unlock()
	t.Cleanup(func() {
		soConfigMu.Lock()
		soEndpoint, soClient = oldEP, oldClient
		soConfigMu.Unlock()
	})
	t.Setenv("EVREN_API_KEY", "test-key")
	t.Setenv("WALSY_SO_MODE", "advisory")
	t.Setenv("HOME", t.TempDir())

	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"state":"FIX421UNIQUE",` + explicitCaps + `}}` + "\n")
	var wg sync.WaitGroup
	var active atomic.Int64
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			active.Add(1)
			defer active.Add(-1)
			// stdio: advisory worker reaches gate.go's SO globals.
			bridgeStdio(bytes.NewBuffer(frame), io.Discard, io.Discard, bytes.NewBuffer(nil))
			// dashboard: same globals through soReleaseComplete.
			soConfigMu.RLock()
			_ = soReleaseComplete()
			soConfigMu.RUnlock()
			// direct SO call: same globals through askSO.
			soConfigMu.RLock()
			_ = askSO(fmt.Sprintf(`{"state":"item %d","need_label":true}`, i))
			soConfigMu.RUnlock()
		}(i)
	}

	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("no SO request entered: decision goroutines stalled")
	}
	if active.Load() == 0 {
		t.Fatal("no decision worker in flight when SO request entered")
	}
	if soConfigMu.TryLock() {
		soConfigMu.Unlock()
		t.Error("SO config rotation acquired Lock while decision goroutines were reading")
	}
	rotated := make(chan struct{})
	go func() {
		soConfigMu.Lock()
		soEndpoint, soClient = fast.URL, fast.Client()
		soConfigMu.Unlock()
		close(rotated)
	}()
	select {
	case <-rotated:
		t.Error("config rotation completed during an in-flight SO read")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	<-rotated
	wg.Wait()
}
