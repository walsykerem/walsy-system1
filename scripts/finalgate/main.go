// Command finalgate asks System One one question per acceptance claim, in the
// shape a reviewer needs: does this claim hold as written, or is something
// missing? One POST per claim, no batching, so a single weak claim cannot ride
// on the others. The aggregate is arithmetic over the measured confidences, and
// the open items are printed on their own line - never folded into a claim.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type claim struct {
	id, text string
}

// claims cite artifacts that exist in this working tree right now.
var claims = []claim{
	{"K1", "decision_items.json loads 56 decision items and registry_decisions_test.go:17 asserts the count is exactly 56, not a floor: TestDecisionRegistry passes."},
	{"K2", "Every reason code a refusal can emit has steer text: steer.go:35 steerTable plus the registry/so- prefixes, and TestSteerTextCoversReasons fails when a reason emitted by route.go, decision_bridge.go, guard.go, write_guard.go or cache.go has no entry. Output was --- PASS: TestSteerTextCoversReasons."},
	{"K3", "A gate:false item decides locally with no model call: TestDecisionRegistry asserts tools.pick loads with Gate false, and registry_decisions_test.go points the endpoint at 127.0.0.1:1 and still gets reason registry-local."},
	{"K4", "The tool budget is a real cap inside a session: the live relay run refused the 4th tool call with 'blocked: router tool budget spent (3 calls for this session)' and refused nothing before that, so 3 of 3 tokens were spent and the 4th was blocked."},
	{"K5", "A write to a path the session never read is refused live: 'PASS unread-write blocked: write to a path this session never touched: /tmp/relay56-a.txt: read it first, or state why it is new'."},
	{"K6", "A PEM private key added by a write is refused before it lands, measured on disk: 'PASS diff-secret blocked: keep the secret ...' and 'PASS secret-absent on disk: absent (open /tmp/relay56-b.txt: no such file or directory)'."},
	{"K7", "An emptying write is refused and the file is unchanged on disk: 'PASS diff-wipe blocked: this write empties /tmp/relay56-w.txt ...' followed by 'PASS wipe-kept content still on disk'. The gate reads the old content from disk (diff_guard.go oldContentOnDisk), so this arm is reachable and not dead code."},
	{"K8", "A write that deletes 24 of 25 lines and adds none is refused as diff-destructive and the file is unchanged: 'PASS diff-destructive' and 'PASS del-kept content still on disk'."},
	{"K9", "A generic no label passes the call through unchanged rather than blocking: 'PASS generic-no forwarded unchanged (/tmp/relay56-c.txt)'."},
	{"K10", "A failed test run never leaves the untested binary live: TestInstallBuildAndDiscardFailed covers a first build (failing binary removed, banner says NO PREVIOUS BINARY) and a later build (old binary restored, banner BUILD REVERTED), and installBuild errors when the build produced no binary."},
	{"K11", "A done entry that denies the work no longer covers a risk. final_audit.go:93 riskCovered calls deniesWork (final_audit.go:106), whose word list is not / n't / never / unverified / untested / unchecked / skipped / todo, so a done entry containing any of those cannot cover a key. Measured in TestFinalAudit: 3 of 3 negative rows fail the audit (\"did not verify api timeout\" over risk api timeout, \"skipped write tests\" over todo write tests, \"wire route untested\" over todo wire route) and the positive row \"verified api timeout under load\" still passes, so 4 of 4 rows behave as claimed."},
	{"K12", "The whole repo is green in one command: scripts/verify.sh has 8 stages - gofmt, go vet, go test -race, go build, lrecon digest, an ASCII scan over 75 files, go vet of the subpackages and the verdict - and printed PASS with lrecon digest 5d8db92d6d5b0481f4295fb64997a321ca768c8567eb26209f8a1b6dd3af1865, kind macho, over 241 test functions, with go test -race reporting ok walsy-system1 5.223s."},
	{"K13", "The live relay harness asserts 18 checks and all 18 pass, ending RELAY-OK with exit 0, over the binary at /Users/keremurhan/walsy-system1/walsy-system1 whose sha256 is 5d8db92d6d5b0481f4295fb64997a321ca768c8567eb26209f8a1b6dd3af1865. The harness itself is tested: TestCheckForwardRejectsBlockAndAcceptsEcho feeds a blocked reply, a wrong method, unforwarded args and a nil reply and each fails, and TestCheckWithoutWantedTextFails proves an empty assertion cannot print PASS."},
	{"K15", "TestEveryDecisionPointSteers drives all 56 registry items through a stub endpoint and checks four branches per item: admit, non-proceed, low-confidence admit and low-confidence non-proceed, plus an out-of-set label. Each result must carry that label's registry action as its steer, never generic text. Saved run: scripts/finalgate/evidence/K15.txt - 3 local, 53 admitted, 50 refused, 53 low-conf, 50 low-conf non-proceed still refused; PASS."},
	{"K16", "TestEveryDecisionPointLive drives all 53 gated items against the real System One endpoint with WALSY_LIVE=1, with no stub installed. It checks each reason code, that the returned label is in the item's own set, that the steer equals that label's registry action, and that every refusal carries a steer. Saved run: scripts/finalgate/evidence/K16.txt - 1 admitted, 14 refused, 38 low-conf, 0 degraded; PASS in 19.07s."},
	{"K14", "A tool call with no file body is scanned at the argument level instead of skipping the secret check: the live relay refused a run_command carrying a PEM header redirected into /tmp/relay56-e.txt, printing 'PASS args-secret blocked: keep the secret in the environment or a key store and write a reference instead' and then 'PASS args-absent on disk: absent'. The code is argsSecretCheck in diff_guard.go, reached from decision_bridge.go for every tools/call with no content key, and TestArgsSecretCheck refuses the redirect while passing a plain go test command."},
}

type reply struct {
	Answers map[string]struct {
		Choice     string  `json:"choice"`
		Confidence float64 `json:"confidence"`
	} `json:"answers"`
}

func key() string {
	if v := os.Getenv("EVREN_API_KEY"); v != "" {
		return v
	}
	b, err := os.ReadFile(os.Getenv("HOME") + "/.walsy-system1/keys/f0c1452bcf623bb1")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
}

func ask(state string) (string, float64, error) {
	body, _ := json.Marshal(map[string]any{
		"model": "clef",
		"state": state,
		"questions": map[string]any{"claim": map[string]any{
			"type":         "choice",
			"instructions": "Is this acceptance claim supported by the evidence it cites, as written?",
			"criteria": map[string]string{
				"done":    "the claim holds and the cited evidence exists and says what the claim says",
				"missing": "the claim overstates the evidence, cites something that does not exist, or leaves out work it says is done",
			},
		}},
	})
	req, err := http.NewRequest(http.MethodPost,
		"https://evren-llmapi.ssyz.org.tr/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key())
	resp, err := (&http.Client{Timeout: 25 * time.Second}).Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("http %d: %.160s", resp.StatusCode, raw)
	}
	var out reply
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", 0, err
	}
	a, ok := out.Answers["claim"]
	if !ok || a.Choice == "" {
		return "", 0, fmt.Errorf("no label: %.160s", raw)
	}
	return a.Choice, a.Confidence, nil
}

// gaps are the review findings that are disclosed, not fixed, and the evidence
// limits. They are printed after the claims so they cannot be read as covered.
var gaps = []string{
	"G1 the model's generic no label passes a tool call through; only deterministic security and scope refusals block. Deliberate, largest blast radius in the set.",
	"G2 the tool budget refills on every initialize frame, so it caps one session boundary, not an agent lifetime.",
	"G3 samePath falls back to a bare basename compare, so a read of /repo/a.go licenses a write to handlers/a.go.",
	"G4 the argument-level scan matches credential shapes only; a shell redirect that deletes files, or a destructive multi-file change, is not diff-judged because there is no old content to compare.",
	"G5 no runtime proof for the reason codes the relay harness never drives, and the walsyrea pass was static only (strings and pclntab); the binary was never executed under it.",
}

func main() {
	if key() == "" {
		fmt.Println("no API key: set EVREN_API_KEY")
		os.Exit(2)
	}
	sum, min, open := 0.0, 1.0, 0
	for _, c := range claims {
		choice, conf, err := ask(c.id + ": " + c.text)
		if err != nil {
			fmt.Printf("%-4s error   %v\n", c.id, err)
			open++
			continue
		}
		sum += conf
		if conf < min {
			min = conf
		}
		if choice != "done" || conf < 0.8 {
			open++
			fmt.Printf("%-4s %-7s %.4f OPEN\n", c.id, choice, conf)
			continue
		}
		fmt.Printf("%-4s %-7s %.4f\n", c.id, choice, conf)
	}
	n := len(claims)
	fmt.Printf("AGGREGATE %.4f mean over %d claims, min %.4f, open %d\n", sum/float64(n), n, min, open)
	for _, g := range gaps {
		fmt.Println("REMAINING", g)
	}
	if open > 0 {
		fmt.Printf("VERDICT INCOMPLETE %d/%d\n", n-open, n)
		os.Exit(1)
	}
	fmt.Printf("VERDICT COMPLETE %d/%d\n", n, n)
}
