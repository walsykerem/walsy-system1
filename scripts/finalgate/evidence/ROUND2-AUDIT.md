# Round 2 — merged ponytail audit (5 agents, read-only)

Repo: /Users/keremurhan/walsy-system1, branch 0.1.0-fix-2, base dc502c8.
Constraints: Go 1.26 stdlib, CGO_ENABLED=0 static binary, no new deps, loopback,
English UI, tests never weakened, key values never printed, no commit until gated.

## MUST findings (all 5 agents merged)

| # | Finding | file:line | Agent |
|---|---------|-----------|-------|
| A1 | routeKey has no salt; cache.go:160 comment claims nonexistent rubricVersion/model/registryVersion. In-memory cache, config fixed at process start => no live staleness. | cache.go:160, route.go:247 | cache |
| A2 | admitMargin=0.10 inert: route.go:376 + decision_bridge.go:115 hardcode Reason:"margin=1"; gate.go drops SO probabilities. Measured law conf>=0.8 needs p1>=0.90 (k=2) / 0.867 (k=3) => argmax gap 0.8/0.733 >> 0.10, margin redundant. | cache.go:18,261-288; gate.go:215-242 | cache |
| A3 | canaryReplay/shouldCanaryReplay only called from tests; production has no known-label `want` source => structurally impossible. | cache.go:298-315 | cache |
| A4 | decision_bridge.go:148 caches EVERY outcome; transient/degraded stored as abstain => same state fails open forever after one SO hiccup. | decision_bridge.go:143-148 | cache |
| A5 | scripts/relay56/TRANSCRIPT.txt stale: harness sha 3eaf86da.. vs actual 931fa0af..; binary sha 0131eef6.. vs actual 5d8db92d6..; 16 PASS vs 18. Replayability claim false. | scripts/relay56/TRANSCRIPT.txt:3,5 | harness |
| A6 | decDeny/decAsk declared, no producer; enforcing "deny" branch unreachable. Advertised hard block does not exist. | route.go:15-16, decision_bridge.go:235 | core |
| A7 | isWriteTool substring match over-triggers. VERIFIED by probe: confirm_plan / format_code / perform_action / transform / normalize / dispatch / readwrite / warm_cache / recreate / rmdir_safe all classified write; enforcing blocks safe calls. | write_guard.go:163-170, :24-27 | safety |
| A8 | Secret-shape scan false negatives: regex misses client_secret, clientSecret, private_key, bare token, AWS_SECRET_ACCESS_KEY; literal floor len>=16 lets 15-char lowercase pass; argsSecretCheck runs only in `else` of writeContent, so content+metadata.api_key escapes. | diff_guard.go:31,117-119; decision_bridge.go:211-218 | safety |

## SHOULD findings

- B1 gateKey hashes raw request bytes (volatile ids fragment cache) — canonicalize to raise hit rate (the pivot's main purpose). decision_bridge.go:45-51.
- B2 gate.go rejects NaN and Conf>1 but not Conf<0. gate.go:237.
- B3 uiPage.Message redundant: template reads .Msg; 4 callers set Message only; fallback copies. Delete field + fallback, change callers. ui.go:31-36,133-134,519,660,665,285.
- B4 dead toast "KEY SAVED". ui.go:119.
- B5 verifyGate/newVerifyGate/patchApplied/recordRun/done tests-only dead; build.go uses only classifyFailure/failureSignature. verify.go:111-141.
- B6 unused CSS .combo-grip/.sheet-foot/.sheet-provider. web/style.css:132,195,196.
- B7 finalgate K1 cites registry_decisions_test.go:17 (comment); assertion at :19-20. scripts/finalgate/main.go:25.
- B8 scripts/verify.sh race stage runs ambient CGO=1 — keep -race, set CGO_ENABLED=1 explicitly with a comment. verify.sh:17-18.
- B9 triplicated exact-56 assertion → one helper. registry_decisions_test.go:19, registry_sweep_test.go:41,235.
- B10 K16 live sweep command absent from VERIFY.md — add.
- B11 steer_test derivedReasons omits diff_guard.go → diff-* reason codes untested for steer coverage. steer_test.go:74.
- B12 isDegraded has no production caller. guard.go:38-40.
- B13 main.go prefixWriter comment says per line, code prefixes per Write. main.go:87-97.
- B14 G5: 6 reason codes have no live relay proof (so-no, so-other, no-state, layer0-static, cache-abstain, negative-verdict); unit-tested only.
- B15 G4 destructive shell redirect not diff-judged — advisory default; accept as documented shortcut.
- B16 G1 generic model "no" passes through — deliberate: gate.go:126-128 defines no as pass-through; wiring_test.go:197 asserts it. A relay deny would invert the gate.
- B17 G2 budget refills on initialize — deliberate session semantics, asserted by decision_bridge_test.go:30.

## YAGNI pruning decision (before System One)

DO (small, removes code or closes real hole): A1 comment, A2 delete dead margin, A3 delete dead canary, A4 stop caching transients, A5 regenerate transcript, A6 delete dead deny branch, A7 boundary-aware write verb, A8 widen secret regex + always scan args, B1-B9, B11-B13.
DOCUMENT, do not change: B15 (G4), B16 (G1), B17 (G2) — intentional, tests assert them.
DEFER with honest residual: B14 (G5) — unit proof exists; extending the live harness for 6 codes is additive machinery, not a correctness fix.
REJECT as gaming: rewording claims to raise a low System One score without changing facts.
