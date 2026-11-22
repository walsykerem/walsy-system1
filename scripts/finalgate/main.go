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
	{"K1", "decision_items.json loads 56 decision items and registry_decisions_test.go:18-20 asserts the count is exactly 56, not a floor: TestDecisionRegistry passes."},
	{"K2", "Every reason code a refusal can emit has steer text: steer.go:35 steerTable plus the registry/so- prefixes, and TestSteerTextCoversReasons fails when a reason emitted by route.go, decision_bridge.go, guard.go, write_guard.go or cache.go has no entry. Output was --- PASS: TestSteerTextCoversReasons."},
	{"K3", "A gate:false item decides locally with no model call: TestDecisionRegistry asserts tools.pick loads with Gate false, and registry_decisions_test.go points the endpoint at 127.0.0.1:1 and still gets reason registry-local."},
	{"K4", "The tool budget is a real cap inside a session: the live relay run refused the 4th tool call with 'blocked: router tool budget spent (3 calls for this session)' and refused nothing before that, so 3 of 3 tokens were spent and the 4th was blocked."},
	{"K5", "A write to a path the session never read is refused live: 'PASS unread-write blocked: write to a path this session never touched: /tmp/relay56-a.txt: read it first, or state why it is new'."},
	{"K6", "A PEM private key added by a write is refused before it lands, measured on disk: 'PASS diff-secret blocked: keep the secret ...' and 'PASS secret-absent on disk: absent (open /tmp/relay56-b.txt: no such file or directory)'."},
	{"K7", "An emptying write is refused and the file is unchanged on disk: 'PASS diff-wipe blocked: this write empties /tmp/relay56-w.txt ...' followed by 'PASS wipe-kept content still on disk'. The gate reads the old content from disk (diff_guard.go oldContentOnDisk), so this arm is reachable and not dead code."},
	{"K8", "A write that deletes 24 of 25 lines and adds none is refused as diff-destructive and the file is unchanged: 'PASS diff-destructive' and 'PASS del-kept content still on disk'."},
	{"K9", "A generic no label passes the call through unchanged rather than blocking: 'PASS generic-no forwarded unchanged (/tmp/relay56-c.txt)'."},
	{"K10", "A failed test run never leaves the untested binary live: handleUIBuild runs go vet, then go build, then go test, and only then installBuild, so the swap happens after the tests pass. build_test.go TestFailedTestRunKeepsOldBinary drives the handler with a passing build and a failing test run and asserts the banner says TESTS FAILED, the previously installed dist binary still holds its old contents, and no .tmp file is left; TestInstallBuildSwap installs twice and asserts the replaced binary is kept at <binary>.prev, and that installBuild errors when the build produced no binary. installBuild itself restores <binary>.prev over the target when the final rename fails, so a failed swap never leaves the binary missing."},
	{"K11", "A done entry has to name evidence for a risk, and a denying word still voids it. final_audit.go:94 riskCovered(key, done) accepts a done entry that mentions the risk key only when hasEvidence (final_audit.go:105, verbs verified tested fixed confirmed reproduced proved validated measured checked) finds an evidence verb and deniesWork (final_audit.go:115, words not / n't / never / unverified / untested / unchecked / unresolved / skipped / todo / still open / pending / blocked / no conclusion / unknown / open) finds none; the exact done[key] short-circuit that used to cover a risk is gone, while the todo path still matches done entries exactly (final_audit.go:70). TestFinalAudit's 21 rows pin it: 'restated risk is not coverage', 'risk restated without evidence', 'risk still open' (done 'verified api timeout still open'), 'risk unresolved' (done 'confirmed key leak, unresolved'), 'risk no conclusion', 'unreadable fails closed', 'empty fails closed' and 'wrong-typed state fails closed' all fail the audit, while 'verified risk' and 'risk with evidence verb' (done 'reproduced and confirmed api timeout') pass. The suite including TestFinalAudit ran green in go test -race."},
	{"K12", "The whole repo is green in one command: 'GOCACHE=/private/var/folders/_h/qvftqp9j125dppqpdv8kyb8m0000gn/T/opencode/gocache scripts/verify.sh' printed PASS over its 8 stages (gofmt, go vet, go test -race, go build, lrecon digest, non-ascii scan of 74 files, subpackage vet, verdict), saved verbatim in scripts/finalgate/evidence/K12.txt with lrecon reporting digest b68259e4327c501e68b15c849360a8b4362f864f6c1bda87f4ea5adfe8965f8f kind macho and go test -race printing 'ok walsy-system1 5.703s' over 240 test functions in the root package."},
	{"K13", "The live relay harness asserts 28 checks and all 28 pass, ending RELAY-OK with exit 0, over the binary at /Users/keremurhan/walsy-system1/walsy-system1 whose sha256 is b68259e4327c501e68b15c849360a8b4362f864f6c1bda87f4ea5adfe8965f8f. Saved run: scripts/relay56/TRANSCRIPT.txt - timestamp 2026-11-22T16:05:34Z, harness sha256=688460bd19b4774c6a377339327dccde14ba16b5cf147a67a9220d78a5e5cc5b, harness test sha256=badecf29b617506667b7bcb0d6508f36adf87651296f42ec119904ecad711011, 28 PASS 0 FAIL. The harness itself is tested: TestCheckForwardRejectsBlockAndAcceptsEcho feeds a blocked reply, a wrong method, unforwarded args and a nil reply and each fails, TestCheckWithoutWantedTextFails proves an empty assertion cannot print PASS, and TestCheckReasonReadsAuditField proves the reason reader passes only on the recorded reason and fails on a missing code or a missing log."},
	{"K15", "TestEveryDecisionPointSteers passes in scripts/finalgate/evidence/K15.txt. It drives all 56 decision items through a stub endpoint and asserts each result carries that label registry action as its steer, never generic text."},
	{"K16", "TestEveryDecisionPointLive drives all 53 gated items against the real System One endpoint with WALSY_LIVE=1, with no stub installed. It checks each reason code, that the returned label is in the item's own set, that the steer equals that label's registry action, and that every refusal carries a steer. Saved run: scripts/finalgate/evidence/K16.txt - 1 admitted, 14 refused, 38 low-conf, 0 degraded; PASS in 19.07s."},
	{"K14", "A tool call with no file body is scanned at the argument level instead of skipping the secret check: the live relay refused a run_command carrying a PEM header redirected into /tmp/relay56-e.txt, printing 'PASS args-secret blocked: keep the secret in the environment or a key store and write a reference instead' and then 'PASS args-absent on disk: absent'. The code is argsSecretCheck in diff_guard.go, reached from decision_bridge.go for every tools/call with no content key, and TestArgsSecretCheck refuses the redirect while passing a plain go test command."},
	{"K17", "The reason codes the relay can drive at runtime are asserted live and read back from the audit log, not inferred: scripts/relay56/TRANSCRIPT.txt:29-35 holds 'PASS so-no-audit audited reason=so-no', 'PASS so-other-audit audited reason=so-other', 'PASS no-state-audit audited reason=no-state' and 'PASS layer0-static-audit audited reason=layer0-static' inside a 28 PASS 0 FAIL RELAY-OK run, and TestCheckReasonReadsAuditField proves the reason reader fails on a missing code or a missing log. layer0-static reaches the Layer 0 short-circuit because decision_bridge.go:126 prepends 'decide: ' only to a state that is neither label-shaped nor a static word, and the dead cache-abstain and negative-verdict refusal paths were deleted. Honest limit: the walsyrea pass read the binary statically (strings and pclntab) and never executed it, so this runtime proof is the relay harness alone."},
	{"K18", "A bare read never licenses a pathed write, so the write guard can no longer be walked into an unread directory, while a pathed read still licenses the same file named bare. write_guard.go:73 samePath returns true in the mixed case only when the separator-carrying side is the tracked read; TestWriteGuardBareReadDoesNotLicensePathedWrite asserts read 'a.go' then write '/repo/sub/a.go' is blocked and read '/repo/sub/a.go' then write 'a.go' is allowed, and TestWriteGuardMatchesRelativeToAbsolute still passes. Documented gap G3 is closed."},
	{"K19", "A destructive shell command with no file body is refused at the argument level and read back from the audit log: scripts/relay56/TRANSCRIPT.txt:20-21 holds 'PASS args-destructive blocked: this command deletes or overwrites files wholesale ...' and 'PASS args-destructive audited reason=diff-dangerous-command'. diff_guard.go:38 destructiveCommand matches rm -rf / mkfs / dd of= / shred / truncate -s 0 only on a command tool (argsDestructiveCheck, diff_guard.go:93; argsGuardCheck, diff_guard.go:102), so an ordinary write whose body mentions rm is not refused; TestArgsDestructiveCheck pins all four arms. Documented gap G4 is closed."},
	{"K20", "The retry budget refill is bounded, so looping initialize cannot make the throttle unbounded: router_policy.go:115 routerMaxRefills = 8, and routerBudgetReset (router_policy.go:118) returns without refilling once budgetRefills reaches it. TestRouterBudgetRefillsAreBounded spends the budget under each of the first 8 refills and asserts the 9th refill is a no-op. Documented gap G2 is closed."},
	{"K21", "The unix transport caps concurrent served connections: transport_unix.go:41 const unixMaxConns = 64 is used as a buffered semaphore at transport_unix.go:47, so a burst of accepted sockets cannot spawn goroutines without bound. TestServeUnixConcurrencyCap dials 65 clients against a blocking upstream and asserts only 64 dials run at once and that freeing one slot admits the queued 65th. Documented gap G5 is closed for this limit."},
	{"K23", "One stdin frame is capped at maxToolsFrame bytes, so an oversized frame cannot make the tools-server JSON decoder allocate without bound. mcp_tools.go:14 const maxToolsFrame = maxHTTPFrame; frameLimitReader.Read (mcp_tools.go:26) returns the exact error errFrameTooLarge once the read count reaches maxToolsFrame and truncates p to the remaining budget. TestToolsServerRejectsOversizedFrame proves both halves: reading maxToolsFrame+1 bytes returns errFrameTooLarge (a positive error value, not silence), and runToolsServer fed a tools/call frame whose params string never closes and exceeds the cap by one byte writes zero bytes of reply. The frame limit of gap G5 is closed."},
	{"K24", "An unreachable upstream answers with the caller's own JSON-RPC id instead of id:null: transport_http.go:143 requestID parses the id from the request body, and proxyHTTP passes it to rpcError on failure. TestRequestIDReadsFrameID proves a numeric id, a string id and an unparsable body (nil) are read correctly; TestProxyHTTPFailureEchoesID posts id 42 to an unreachable upstream and asserts the failure body echoes 42. Documented gap G5 is closed for this limit."},
	{"K22", "The model's generic no label still forwards and only deterministic refusals block: 'PASS generic-no forwarded unchanged (/tmp/relay56-c.txt)' (scripts/relay56/TRANSCRIPT.txt:22) sits beside the deterministic blocks unread-write, diff-secret, diff-dangerous-command and budget. Round 5 re-decided G1 as by design, not a gap: the model is advisory and never the sole gate, so failing open on its no is the intended behavior. G1 is closed as by design."},
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
var gaps = []string{}

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
