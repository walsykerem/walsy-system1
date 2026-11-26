package app

import (
	"reflect"
	"testing"
)

func TestTodoLedgerFrom(t *testing.T) {
	cases := []struct {
		name      string
		state     string
		choice    string
		conf      float64
		uncovered []string
	}{
		{
			name:   "full coverage allows done",
			state:  `{"todo":["write tests","validate json"],"done":["write tests","validate json"]}`,
			choice: "complete",
			conf:   1,
		},
		{
			name:      "partial coverage blocks done",
			state:     `{"todo":["write tests","validate json","run go test"],"done":["write tests"]}`,
			choice:    "do_more",
			conf:      1.0 / 3.0,
			uncovered: []string{"validate json", "run go test"},
		},
		{
			name:   "empty todo is no opinion",
			state:  `{"todo":[],"done":["anything"]}`,
			choice: "",
		},
		{
			name:   "missing todo is no opinion",
			state:  `{"done":["anything"]}`,
			choice: "",
		},
		{
			name:   "malformed json fails open",
			state:  `{"todo":["write tests"`,
			choice: "",
		},
		{
			name:   "wrong field type fails open",
			state:  `{"todo":"write tests","done":[]}`,
			choice: "",
		},
		{
			name:      "unverified named risk blocks done",
			state:     `{"todo":["risk: untested migration path"],"done":["wrote handler"]}`,
			choice:    "do_more",
			conf:      0,
			uncovered: []string{"risk: untested migration path"},
		},
		{
			name:   "case and whitespace fold",
			state:  `{"todo":["Write   Tests"],"done":[" write tests "]}`,
			choice: "complete",
			conf:   1,
		},
		{
			name:      "duplicate todo counts once",
			state:     `{"todo":["a","A","b"],"done":["a"]}`,
			choice:    "do_more",
			conf:      0.5,
			uncovered: []string{"b"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := todoLedgerFrom(c.state)
			if got.Verdict.Choice != c.choice {
				t.Fatalf("choice = %q, want %q (ledger %+v)", got.Verdict.Choice, c.choice, got)
			}
			if c.choice != "" && got.Verdict.Conf != c.conf {
				t.Errorf("conf = %v, want %v", got.Verdict.Conf, c.conf)
			}
			if !reflect.DeepEqual(got.Uncovered, c.uncovered) {
				t.Errorf("uncovered = %v, want %v", got.Uncovered, c.uncovered)
			}
		})
	}
}

// A done claim over an unreadable state must not be refused by the ledger.
func TestTodoLedgerFailsOpen(t *testing.T) {
	for _, state := range []string{``, `not json`, `[]`, `{"todo":null,"done":null}`} {
		if got := todoLedgerFrom(state); got.Verdict.Choice != "" || len(got.Uncovered) != 0 {
			t.Errorf("state %q must yield no verdict, got %+v", state, got)
		}
	}
}
