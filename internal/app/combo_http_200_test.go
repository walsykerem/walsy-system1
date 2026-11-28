package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestComboSOEndpoint200WithAnswersIsOK(t *testing.T) {
	resetRouterForTest(t)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"probe":{"choice":"yes","confidence":0.9}}}`))
	}))
	defer s.Close()
	got := RunCombo([]ComboStep{{Name: "Evren System One/clef", Base: s.URL + "/v1/systemone", Key: "sk-test"}}, 0)
	if !got.Steps[0].OK || got.Steps[0].Detail != "key accepted" {
		t.Fatalf("valid SO answer should be OK: %+v", got.Steps[0])
	}
}

func TestComboSOEndpoint200MalformedBodyNotOK(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"bad_json", `{"not_json":`},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetRouterForTest(t)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			got := RunCombo([]ComboStep{{Name: "Evren System One/clef", Base: s.URL + "/v1/systemone", Key: "sk-test"}}, 0)
			if got.Steps[0].OK || got.Steps[0].Detail != "invalid system one response" {
				t.Fatalf("malformed SO answer must fail: %+v", got.Steps[0])
			}
		})
	}
}

func TestComboNonSOEndpoint200PlainTextStillOK(t *testing.T) {
	resetRouterForTest(t)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("plain text"))
	}))
	defer s.Close()
	got := RunCombo([]ComboStep{{Name: "x", Base: s.URL}}, 0)
	if !got.Steps[0].OK {
		t.Fatalf("generic 200 probe must stay reachable: %+v", got.Steps[0])
	}
}
