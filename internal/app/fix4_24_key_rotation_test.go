package app

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
)

// Atomic key rotation must be one swap, not the UI's add-then-remove pair:
// a request during rotation sees only the old key or the new key, never both
// and never a half-written list.
func TestFix4_24AtomicKeyRotationUnderConcurrentRequests(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("EVREN_API_KEY", "")
	t.Setenv("SIM_KEY", "")
	const endpoint = "http://127.0.0.1:1/v1"
	savedEndpoint := soEndpoint
	soEndpoint = endpoint
	defer func() { soEndpoint = savedEndpoint }()

	const (
		name   = "SIM/A14-ROTATION"
		oldKey = "rotation-old-key"
		newKey = "rotation-new-key"
	)
	if err := writeRegistryLines([]provider{{Name: name, Base: endpoint, KeyEnv: "SIM_KEY"}}); err != nil {
		t.Fatal(err)
	}
	if err := writeKeyList(name, []string{oldKey}); err != nil {
		t.Fatal(err)
	}

	const requests = 50
	start, stop := make(chan struct{}), make(chan struct{})
	oldSeen, newSeen := make(chan struct{}, 1), make(chan struct{}, 1)
	bad := make(chan string, requests)
	var wg sync.WaitGroup
	wg.Add(requests)
	for range requests {
		go func() {
			defer wg.Done()
			<-start
			reported := false
			for {
				select {
				case <-stop:
					return
				default:
				}
				keys, err := loadKeys(name)
				if err != nil && !reported {
					bad <- fmt.Sprintf("loadKeys: %v", err)
					reported = true
				}
				if len(keys) != 1 || (keys[0] != oldKey && keys[0] != newKey) {
					if !reported {
						bad <- fmt.Sprintf("mixed key list %q", keys)
						reported = true
					}
				}
				got := soAPIKey()
				if got != oldKey && got != newKey && !reported {
					bad <- fmt.Sprintf("selected key %q is neither old nor new", got)
					reported = true
				}
				seen := oldSeen
				if got == newKey {
					seen = newSeen
				}
				select {
				case seen <- struct{}{}:
				default:
				}
				runtime.Gosched()
			}
		}()
	}
	close(start)
	<-oldSeen
	if err := writeKeyList(name, []string{newKey}); err != nil {
		t.Fatal(err)
	}
	<-newSeen
	close(stop)
	wg.Wait()
	close(bad)
	for msg := range bad {
		t.Error(msg)
	}

	keys, err := loadKeys(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != newKey {
		t.Fatalf("after rotation key list = %q, want [%s]", keys, newKey)
	}
	if got := soAPIKey(); got != newKey {
		t.Fatalf("after rotation selected key = %q, want %s", got, newKey)
	}
}
