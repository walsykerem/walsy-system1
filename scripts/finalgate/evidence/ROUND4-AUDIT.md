# ROUND 4 AUDIT — branch 1.0.0-fix-4 (base 680cb11)

Three read-only scanner agents (decision core / transport+security / UI+build) read the
tree at 680cb11 and returned one table each. Counts: **6 MUST, 16 SHOULD, 4 LOW**.
Outcome: **6/6 MUST fixed, 15/16 SHOULD fixed, 1 SHOULD deliberately reverted** (its
residual is the pre-existing documented gap G3), **4 LOW documented, not fixed**.

## MUST (all fixed)

| # | Where | Problem | Fix |
|---|-------|---------|-----|
| M1 | guard_test.go:47-57 | `TestClassify503NeverReturnsStatus` never wrote `rec`, so it asserted nothing | test deleted |
| M2 | final_audit.go:93-103 | `riskCovered` accepted a mere substring match as coverage | now requires an evidence verb (`hasEvidence`) and no denying word (`deniesWork`); the exact `done[key]` short-circuit removed |
| M3 | final_audit.go:33-35 | unparseable or wrong-typed state returned `OK: true` (fail-open) | returns `Missing: ["state unreadable"]` (fail-closed) |
| M4 | build.go:255-258, 183-192 | a failed install left the binary swapped away with no restore | `installBuild` keeps `<binary>.prev` and renames it back when the final rename fails |
| M5 | build.go:255 vs 259 | `installBuild` ran before `runGoTest`, so a failing test left an untested binary live | tests run first; on failure the `.tmp` is removed and the old binary is untouched (`discardFailedBuild` deleted as dead) |
| M6 | dashboard_test.go:654 | `TestComboWeightRoundTrip` wrote the real `~/.walsy-system1/combo.txt` (BUILD runs `go test`) | `t.Setenv("HOME", t.TempDir())` |

## SHOULD (15 fixed, 1 reverted)

| # | Where | Problem | Fix |
|---|-------|---------|-----|
| S1 | decision_bridge.go:78-112 | six `if cap.X { obj[...]=true }` blocks were fully overridden by the loop below them | `buildStateWithCapability(request, cap)` → `buildSOState(request)`; the blocks deleted. `capabilityConfig`/`soCapability`/`gateKey` kept: they still feed `gateKey` and two acceptance tests |
| S2 | gate.go:157-160 | inert `threshold` parameter on `askSOQWithThreshold` | merged into `askSOQ(state, question)` |
| S3 | cache_test.go:199-201 | three `_ =` statements asserted nothing | deleted |
| S4 | write_guard.go:95-98 | a write whose path could not be read returned pass | refused: `write with no readable path: …` (reason `unread-path`) |
| S5 | write_guard.go:70-79 | `samePath` basename fallback over-licenses mixed paths | **reverted.** Requiring both paths to carry a slash broke `TestWriteGuardMatchesRelativeToAbsolute` (read `/repo/sub/a.go`, write `a.go` is a legitimate flow). The guard is a canary — `diff-secret`/`diff-wipe` still judge the content — so the fallback stays and the residual is gap G3, unchanged |
| S6 | registry_decisions.go:9 | comment said 41 items, registry has 56 | comment fixed |
| S7 | router_policy.go:53-57 | `routerAuditKeys` lacked `method` and `allow`, both emitted by callers | added |
| S8 | metric.go:26 | dead `KeyHash` field | deleted |
| S9 | transport_unix_test.go:59-79 | the test could not fail: it never observed the server draining | half-closes the socket and asserts the read ends in `io.EOF`, not an error frame |
| S10 | calibrate.go:13-144 | five dead functions (`calibratedConfidence`, `ece`, `isotonicFit`, `quantileThreshold`, `rZero`) | deleted with `calibrate_test.go` and `TestCalibrationECE`; file is now `defaultThreshold() 0.8`, which `route.go:280` and `mcp_tools.go:163` actually use |
| S11 | dashboard.go, ui.go | `DocsURL` plumbing rendered nowhere | deleted from dashboard.go, ui.go and its two test assertions |
| S12 | build_test.go:136-189 | a comment promised safety no test checked | real tests added with M5 (`TestFailedTestRunKeepsOldBinary`, `TestInstallBuildSwap`) |
| S13 | final_audit_test.go:24, 28-29 | pinned the weak behaviour and expected fail-open | rows flipped, negative cases added (restatement, still-open, unresolved, no-conclusion, wrong type, unreadable) |
| S14 | web/style.css:146-152, 100-106, 194 | `.chip` and `.field` rules matched nothing | deleted; `.chips` kept (live in web/index.html:204, 210) |
| S15 | ui_form_test.go:111 | comment said clamp 2000..30000, code is 1000..30000 | comment fixed |
| S16 | sim_test.go:165 | dead `_ = s` | deleted |

Extra (found during the sweep, production-dead): `router_policy.go:194 discoveredTools`,
`routerState.tools` and the `WALSY_ROUTER_TOOLS` parse deleted. The env var was read by
nothing else in the tree, so it was a silently inert config.

## LOW — documented, not fixed

1. `transport_unix.go:43-59` starts an unbounded goroutine per accepted connection.
2. `mcp_tools.go:17-27` decodes stdin with no size limit (`io.LimitReader` missing).
3. `transport_http.go:141` — the upstream-unavailable error carries `id: null`.

They are limits of a loopback single-user tool, not defects reachable through its
documented use; none of them was in scope for a YAGNI prune, and fixing them would add
code the product does not need yet.

## Verification

- `gofmt` clean, `go vet` clean, `go test -count=1 ./...` green (233 test functions in the
  root package), `go test -race` green.
- `scripts/verify.sh`: 8/8 PASS (gofmt, vet, `-race`, build, lrecon digest, non-ascii scan
  of 74 files, subpackage vet, verdict) — saved verbatim in `K12.txt`.
- `scripts/relay56`: 26 PASS, 0 FAIL, RELAY-OK against the rebuilt binary, transcript
  regenerated with the new binary sha.