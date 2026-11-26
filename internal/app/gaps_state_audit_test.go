package app

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func gapStateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func gapStateWriteState(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

// Startup refusal is the only place an unset SO mode stops the process.
func TestGapStateSOModeStartupRefusals(t *testing.T) {
	cases := []struct {
		name, file, envMode, envApprover, wantErr string
	}{
		{"unset", "", "", "", "WALSY_SO_MODE unset or invalid"},
		{"invalid both", "so_mode=bogus\n", "BOGUS", "", "WALSY_SO_MODE unset or invalid"},
		{"advisory file", "so_mode=advisory\n", "", "", ""},
		{"invalid env falls back to file", "so_mode=advisory\n", "bogus", "", ""},
		{"advisory env beats file", "so_mode=enforcing\n", " ADVISORY ", "", ""},
		{"enforcing needs approver", "so_mode=enforcing\n", "", "", "requires a named approver"},
		{"blank approver refused", "so_mode=enforcing\napprover=   \n", "", "", "requires a named approver"},
		{"config approver", "so_mode=enforcing\napprover=Operator\n", "", "", ""},
		{"env approver", "so_mode=enforcing\n", "", "Operator", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := gapStateHome(t)
			gapStateWriteState(t, filepath.Join(home, ".walsy-system1", "config.txt"), []byte(c.file))
			t.Setenv("WALSY_SO_MODE", c.envMode)
			t.Setenv("WALSY_SO_APPROVER", c.envApprover)
			err := soModeStartupError()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("startup refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("startup err = %v, want %q", err, c.wantErr)
			}
		})
	}
}

// Config parsing must ignore junk without turning it into a key, and must not
// read a permissive or symlinked config as an explicit opt-in.
func TestGapStateReadSOConfigIgnoresMalformedLines(t *testing.T) {
	home := gapStateHome(t)
	path := filepath.Join(home, ".walsy-system1", "config.txt")
	longKey := "long" + strings.Repeat("k", 65)
	gapStateWriteState(t, path, []byte(strings.Join([]string{
		"# comment",
		"so_mode = advisory ",
		"note without equals",
		" = empty key",
		longKey + "=dropped",
		"dup=first",
		"dup= second ",
		"value=a=b=c",
		"blank=   ",
	}, "\r\n")))

	got := readSOConfig()
	want := map[string]string{"so_mode": "advisory", "dup": "second", "value": "a=b=c", "blank": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("config = %#v, want %#v", got, want)
	}

	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if got := readSOConfig(); len(got) != 0 {
		t.Fatalf("permissive config read as %#v", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("so_mode=enforcing\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if got := readSOConfig(); len(got) != 0 {
		t.Fatalf("symlinked config read as %#v", got)
	}
}

// Pruning is a direct storage concern: weights must survive, malformed lines
// must not be resurrected, and a no-op must not rewrite the file.
func TestGapStatePruneComboModelAndProvider(t *testing.T) {
	home := gapStateHome(t)
	path := filepath.Join(home, ".walsy-system1", "combo.txt")
	original := "P\tkeep\t2\nP\tdrop\t3\nQ\tkeep\nP\tdrop\tnotint\nDEMO/x\tdrop\nbadline\n"
	gapStateWriteState(t, path, []byte(original))

	if err := pruneCombo("P", "drop"); err != nil {
		t.Fatal(err)
	}
	steps, err := loadCombo()
	if err != nil {
		t.Fatal(err)
	}
	want := []stateStep{{Provider: "P", Model: "keep", Weight: 2}, {Provider: "Q", Model: "keep"}}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("steps after model prune = %#v, want %#v", steps, want)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "P\tkeep\t2\nQ\tkeep\n" {
		t.Fatalf("persisted combo = %q", raw)
	}

	gapStateWriteState(t, path, []byte(original))
	if err := pruneCombo("P", ""); err != nil {
		t.Fatal(err)
	}
	steps, err = loadCombo()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(steps, []stateStep{{Provider: "Q", Model: "keep"}}) {
		t.Fatalf("steps after provider prune = %#v", steps)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := pruneCombo("MISSING", "model"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("no-op prune rewrote file: %q -> %q", before, after)
	}
}

// Distinct refusal reasons matter to the UI, and a removed key must leave no
// slot behind.
func TestGapStateKeyListRefusalsAndSlots(t *testing.T) {
	gapStateHome(t)
	const name = "Gap Provider"
	wantSlots := make([]int, maxKeysPerProvider)
	for i := 1; i <= maxKeysPerProvider; i++ {
		wantSlots[i-1] = i
		if err := addKey(name, fmt.Sprintf("key-%d", i)); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if slots := keySlots(name); !reflect.DeepEqual(slots, wantSlots) {
		t.Fatalf("slots = %v, want %v", slots, wantSlots)
	}
	if err := addKey(name, "key-9"); !errors.Is(err, errKeyLimit) {
		t.Fatalf("overflow err = %v, want errKeyLimit", err)
	}
	if err := addKey(name, "key-1"); !errors.Is(err, errKeyDuplicate) {
		t.Fatalf("duplicate err = %v, want errKeyDuplicate", err)
	}
	if err := addKey(name, "bad\rkey"); err == nil || errors.Is(err, errKeyDuplicate) || errors.Is(err, errKeyLimit) {
		t.Fatalf("separator key err = %v", err)
	}
	for _, idx := range []int{-1, maxKeysPerProvider} {
		if err := removeKeyAt(name, idx); err == nil || !strings.Contains(err.Error(), "invalid key index") {
			t.Fatalf("index %d err = %v", idx, err)
		}
	}
	if err := removeKeyAt(name, 0); err != nil {
		t.Fatal(err)
	}
	keys, err := loadKeys(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != maxKeysPerProvider-1 || keys[0] != "key-2" {
		t.Fatalf("keys after removal = %v", keys)
	}
	if slots := keySlots(name); len(slots) != maxKeysPerProvider-1 || slots[0] != 1 {
		t.Fatalf("slots after removal = %v", slots)
	}
	if err := writeKeyList(name, nil); err != nil {
		t.Fatal(err)
	}
	if n := keyCount(name); n != 0 {
		t.Fatalf("key count after clear = %d", n)
	}
	path, err := keyFilePath(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleared key file still exists: %v", err)
	}
}

// State writes are confined to the state dir and its 0700 keys dir.
func TestGapStateWriteStateFileRefusesForeignPaths(t *testing.T) {
	t.Run("foreign and nested paths", func(t *testing.T) {
		gapStateHome(t)
		dir, err := stateDir()
		if err != nil {
			t.Fatal(err)
		}
		keys := filepath.Join(dir, "keys")
		if err := os.MkdirAll(keys, 0700); err != nil {
			t.Fatal(err)
		}
		cases := []struct {
			name, path, want string
		}{
			{"outside state dir", filepath.Join(t.TempDir(), "foreign.txt"), "state path rejected"},
			{"nested dir", filepath.Join(dir, "nested", "f.txt"), "state path rejected"},
			{"target is a dir", keys, "state target rejected"},
		}
		for _, c := range cases {
			err := writeStateFile(c.path, []byte("x"))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
			}
		}
	})

	t.Run("keys dir symlink and mode", func(t *testing.T) {
		home := gapStateHome(t)
		_ = home
		dir, err := stateDir()
		if err != nil {
			t.Fatal(err)
		}
		realKeys := filepath.Join(t.TempDir(), "keys")
		if err := os.Mkdir(realKeys, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realKeys, filepath.Join(dir, "keys")); err != nil {
			t.Fatal(err)
		}
		if err := writeStateFile(filepath.Join(dir, "keys", "f.txt"), []byte("x")); err == nil || !strings.Contains(err.Error(), "keys dir rejected") {
			t.Fatalf("symlinked keys dir err = %v", err)
		}
	})

	t.Run("keys dir permissive mode", func(t *testing.T) {
		gapStateHome(t)
		dir, err := stateDir()
		if err != nil {
			t.Fatal(err)
		}
		keys := filepath.Join(dir, "keys")
		if err := os.Mkdir(keys, 0755); err != nil {
			t.Fatal(err)
		}
		if err := writeStateFile(filepath.Join(keys, "f.txt"), []byte("x")); err == nil || !strings.Contains(err.Error(), "keys dir rejected") {
			t.Fatalf("permissive keys dir err = %v", err)
		}
	})
}

// Untrusted prompt delimiters must be neutralized before the state is sent.
func TestGapStateEscapePromptDelimiters(t *testing.T) {
	in := "keep [INST] then <|system|> and <<tool>> plus [/INST] end"
	want := "keep [DELIMITER] then [DELIMITER] and [DELIMITER] plus [DELIMITER] end"
	if got := escapePromptDelimiters(in); got != want {
		t.Fatalf("escaped = %q, want %q", got, want)
	}
	if got := escapePromptDelimiters("a < b > c"); got != "a < b > c" {
		t.Fatalf("plain angle brackets changed: %q", got)
	}
}

// A restarted chain must reject edits to any earlier body, not merely trust the
// sequence numbers, and must remember which frames already have a decision.
func TestGapStateReceiptChainLoadFailsClosed(t *testing.T) {
	seed := func(t *testing.T) (string, []string) {
		t.Helper()
		gapStateHome(t)
		c := &receiptChain{}
		if _, err := c.appendReceipt(receipt{Event: "decision", Decision: decPin, Reason: "so-admit", Method: "tools/call"}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.appendReceipt(receipt{Event: "decision", Decision: decPass, Reason: "so-no"}); err != nil {
			t.Fatal(err)
		}
		path, err := receiptStorePath()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return path, strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
	rewrite := func(t *testing.T, path string, lines []string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	editLine := func(t *testing.T, line string, change func(r *receipt)) string {
		t.Helper()
		var r receipt
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		change(&r)
		b, err := canonicalJSON(r)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	cases := []struct {
		name   string
		mutate func(t *testing.T, path string, lines []string)
	}{
		{"tampered earlier body", func(t *testing.T, path string, lines []string) {
			lines[0] = editLine(t, lines[0], func(r *receipt) { r.Reason = "tampered" })
			rewrite(t, path, lines)
		}},
		{"bad sequence", func(t *testing.T, path string, lines []string) {
			lines[1] = editLine(t, lines[1], func(r *receipt) { r.Seq = 3 })
			rewrite(t, path, lines)
		}},
		{"bad prev", func(t *testing.T, path string, lines []string) {
			lines[1] = editLine(t, lines[1], func(r *receipt) { r.Prev = strings.Repeat("0", 64) })
			rewrite(t, path, lines)
		}},
		{"truncated tail", func(t *testing.T, path string, lines []string) {
			rewrite(t, path, append(lines, "{"))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path, lines := seed(t)
			c.mutate(t, path, lines)
			if got := loadReceiptChain(); got != nil {
				t.Fatalf("tampered chain loaded: %#v", got)
			}
		})
	}

	t.Run("frames dedup outbound mirror", func(t *testing.T) {
		gapStateHome(t)
		c := &receiptChain{}
		head, err := c.appendReceipt(receipt{Event: "decision", FrameID: "gap-frame", Decision: decPin})
		if err != nil {
			t.Fatal(err)
		}
		loaded := loadReceiptChain()
		if loaded == nil || loaded.seq != 1 || hex.EncodeToString(loaded.head) != head {
			t.Fatalf("valid chain not loaded: %#v head=%s", loaded, head)
		}
		path, err := receiptStorePath()
		if err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := loaded.appendReceipt(receipt{Event: "outbound_response", FrameID: "gap-frame", Decision: decPass})
		if err != nil {
			t.Fatal(err)
		}
		if got != head {
			t.Fatalf("mirror head = %s, want %s", got, head)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("outbound mirror appended a second receipt")
		}
	})
}

// The evidence link must point at the same inode, never a foreign ledger.
func TestGapStateReceiptEvidenceLinkFailClosed(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "store.chain")
	evidence := filepath.Join(dir, "evidence", "receipt.chain")
	if err := os.WriteFile(store, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(evidence), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidence, []byte("foreign\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := linkReceiptEvidence(store, evidence); !errors.Is(err, errReceiptUnsafeFile) {
		t.Fatalf("foreign evidence err = %v", err)
	}
	if err := os.Remove(evidence); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store, evidence); err != nil {
		t.Fatal(err)
	}
	if err := linkReceiptEvidence(store, evidence); !errors.Is(err, errReceiptUnsafeFile) {
		t.Fatalf("symlinked evidence err = %v", err)
	}
	if err := os.Remove(evidence); err != nil {
		t.Fatal(err)
	}
	if err := linkReceiptEvidence(store, evidence); err != nil {
		t.Fatal(err)
	}
	src, err := os.Lstat(store)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := os.Lstat(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(src, dst) {
		t.Fatal("evidence link is not the stored chain")
	}
}

// Registry promotion must reject record-breaking fields before writing a temp
// file or replacing the live registry.
func TestGapStateRegistryWriteRejectsUnsafeFields(t *testing.T) {
	home := gapStateHome(t)
	base := "https://a.example/v1"
	cases := []struct {
		name string
		p    provider
	}{
		{"negative cost", provider{Name: "A", Base: base, KeyEnv: "A_KEY", Cost: -1}},
		{"control byte", provider{Name: "A\nB", Base: base, KeyEnv: "A_KEY"}},
		{"comma in key env", provider{Name: "A", Base: base, KeyEnv: "A,KEY"}},
		{"demo name", provider{Name: "DEMO/A", Base: base, KeyEnv: "A_KEY"}},
		{"remote plaintext", provider{Name: "A", Base: "http://a.example/v1", KeyEnv: "A_KEY"}},
		{"long name", provider{Name: strings.Repeat("A", maxProviderNameLen+1), Base: base, KeyEnv: "A_KEY"}},
	}
	for _, c := range cases {
		if err := writeRegistryLines([]provider{c.p}); err == nil || err.Error() != "invalid field" {
			t.Errorf("%s: err = %v, want invalid field", c.name, err)
		}
	}
	path := filepath.Join(home, ".walsy-system1", "providers.txt")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed writes replaced registry: %v", err)
	}
}

// Guard codes and tool-name boundaries are audited, so their mapping must be
// exact rather than derived from a source regex.
// Secret shapes outrank destructive ones, and the regex must catch the
// non-rm overwrite forms on command tools only.
func TestGapStateArgsGuardPrecedenceAndDestructiveVariants(t *testing.T) {
	for _, command := range []string{
		"mkfs.ext4 /dev/x",
		"shred -u /tmp/x",
		"truncate -s 0 /tmp/x",
	} {
		args, err := json.Marshal(map[string]string{"command": command})
		if err != nil {
			t.Fatal(err)
		}
		if got := argsDestructiveCheck("bash", args); !got.Refuse || got.Code != "diff-dangerous-command" {
			t.Errorf("%q: verdict = %+v", command, got)
		}
		if got := argsDestructiveCheck("write_file", args); got.Refuse {
			t.Errorf("non-command tool refused %q", command)
		}
	}
	both := json.RawMessage(`{"command":"rm -rf /; -----BEGIN RSA PRIVATE KEY-----"}`)
	if got := argsGuardCheck("bash", both); !got.Refuse || got.Code != "diff-secret" {
		t.Fatalf("secret precedence = %+v, want diff-secret", got)
	}
	literals := []struct{ key, val, want string }{
		{"token", "short", ""},
		{"token", "${API_KEY}", ""},
		{"token", "your", ""},
		{"token", "yoursecret", "token assigned a literal value"},
	}
	for _, c := range literals {
		if got := literalSecretKind(c.key, c.val); got != c.want {
			t.Errorf("literalSecretKind(%q,%q) = %q, want %q", c.key, c.val, got, c.want)
		}
	}
	added, removed := diffLines("a\r\nb\n", "a\nb\r\n")
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("CRLF line endings counted as a change: added=%v removed=%v", added, removed)
	}
}

// A short risk key must not be covered by a substring of a longer word, and
// repeated aliases across ledger lists must collapse to one missing item.
func TestGapStateFinalAuditWordBoundariesAndRiskDedup(t *testing.T) {
	if mentionsKey("verified tests", "test") {
		t.Fatal("test covered by tests")
	}
	if mentionsKey("verified _test_", "test") {
		t.Fatal("test covered inside underscores")
	}
	if mentionsKey("verified 3test", "test") {
		t.Fatal("test covered after a digit")
	}
	if !mentionsKey("verified test suite", "test") {
		t.Fatal("test at a word boundary not recognized")
	}
	clean := finalAudit(`{"todo":["risk: test"],"risk":["test"],"risks":["TEST"],"done":["verified test suite"]}`)
	if !clean.OK || len(clean.Missing) != 0 {
		t.Fatalf("word-boundary audit = %+v", clean)
	}
	dup := finalAudit(`{"risk":["api timeout","API  TIMEOUT"],"unverified_risks":["api timeout"],"done":[]}`)
	if dup.OK || len(dup.Missing) != 1 || dup.Missing[0] != "risk: api timeout" {
		t.Fatalf("deduplicated risk audit = %+v", dup)
	}
}

// Rune-counted line budgets need an explicit boundary assertion.
func TestGapStateDistillLineRuneBoundary(t *testing.T) {
	// Build the multibyte rune at runtime so the test source stays ASCII-clean.
	multi := string(rune(0xE9))
	atCap := strings.Repeat(multi, maxSOEvidenceLineChars)
	out, ok := distillSOState(StateSkeleton{State: atCap})
	if !ok || len(out.Evidence) != 1 || utf8.RuneCountInString(out.Evidence[0]) != maxSOEvidenceLineChars {
		t.Fatalf("at-cap rune line dropped: %#v ok=%v", out, ok)
	}
	over := atCap + multi
	if got, ok := distillSOState(StateSkeleton{State: over}); ok || len(got.Evidence) != 0 {
		t.Fatalf("over-cap rune line kept: %#v ok=%v", got, ok)
	}
}
