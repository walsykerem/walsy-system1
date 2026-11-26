# Round 3 — merged ponytail audit (3 agents, read-only)

Repo: /Users/keremurhan/walsy-system1, branch 0.1.0-fix-3, base 83285bf (clean).
Constraints: Go 1.26 stdlib, CGO_ENABLED=0 static binary, no new deps, loopback,
English UI, tests never weakened, key values never printed, no commit until gated.

Agents: core gates G1-G5; guards/cache/state; harness/docs/UI. All read-only at HEAD.

## MUST findings

- M1 (core 0.92) `decision_bridge.go:290-297`: `steerFailOpen` omits `so-low-conf`, but `route.go:362-370` returns decPass with a non-empty steer for it, so `steerBlocks()` blocks in enforcing. Violates `AGENT_CONTRACT.md:30` / `README.md:47-48`. K16 measured 38/53 low-conf. Fix: add one map entry + regression test.
- M2 (harness 0.85 + guards 0.8, same root) `diff_guard.go:33`: `diffSecretAssign` requires `\s*[:=]` right after the key word. JSON has a closing quote, so `{"password":"hunter2pass"}` is not matched by the assignment arm. Fix: allow optional (escaped) quotes around key; add test case.
- M3 (guards 0.9) `diff_guard.go:114`: `literalSecretKind` uses `strings.Contains` for skip words, so a real secret containing `$`, `your`, `example`, `os.` etc. passes (`hunter2$ecret`). Fix: `strings.HasPrefix` only; add test case.
- M4 (guards 0.85) `write_guard.go:163-175`: `isWriteTool` lowercases before splitting, so camelCase `writeFile`/`editFile`/`deleteFile` skip the unread-path guard. Fix: split camelCase on the original string; add test cases.

## SHOULD findings

- S1 (core 0.8) `decision_bridge.go:254-256`: `routerAllow` deny list only logged; enforcing never refuses a denied tool. Fix: refuse in `enforcingGate`, enforcing only + test.
- S2 (guards 0.8) `decision_bridge.go:317-322`: advisory path skips `argsSecretCheck`, so it cannot show what enforcing would block. Fix: add the same audit call.
- S3 (guards 0.7) `write_guard.go:95-102`: repeat counter increments for every `tools/call`, so 9 identical reads block. Fix: count only write tools.
- S4 (harness 0.9) `VERIFY.md:15-22` lists 6 stale commands, :26 digest historical vs verify.sh 8 stages / `30cb4b0c…`. Doc sync.
- S5 (harness 0.9) `dashboard.go:33` `dashboardProvider.Message` dead, sole writer `ui.go:208-210`. Delete field + loop.
- S6 (harness 0.95) `web/style.css:126-127` `.btn-danger` unused. One-rule prune.
- S7 (guards 0.7; round-2 G3) `write_guard.go:70-79` `samePath` basename/suffix authorizes unrelated absolute files. Round 2 decided DOCUMENT; still disclosed in finalgate G3.
- S8 (round 2 B14/G5) six reason codes have unit proof but no live relay proof. Deferred as additive harness work.

## Documented, no change

- G1 generic no passes: intentional, `gate.go:125-128`, `wiring_test.go:197-204`.
- G2 budget refills on initialize: intentional session boundary, `decision_bridge_test.go:58-62`.
- G3 samePath: deliberate/tested shortcut, `write_guard.go:70-79`, `write_guard_test.go:35-36`; kept documented.
- G4 destructive redirect not diff-judged: documented shortcut, `diff_guard.go:39,60,76`.
- Core 8-10: inert `askSOQWithThreshold` clamp; bridge cache drops Label/Steer on admitted pin (no consumer); `deniesWork` heuristic (fail-closed). No live defect.
- Guards 7-10: `build.go` tmp residue, `verify.go` bucket order, coarse `keytest.go`, dead singleflight group (tests depend on it).
- Harness D3: all round-2 claim/hash drift checks pass; transcript current.

## YAGNI pruning decision (before System One)

DO: M1, M2, M3, M4, S1, S2, S3, S4, S5, S6 — all small, close real holes or delete dead code, each with a test or doc proof.
DOCUMENT, do not invert: G1, G2, G3/S7 (round-2 decision stands), G4.
DEFER honestly: S8/B14 live proof for six reason codes — additive harness, unit-tested already.
REJECT as gaming: rewording claims or gap labels to raise a score without changing facts.

Not checked by agents: go test/build, live relay, walsyrea execution, System One gate.
