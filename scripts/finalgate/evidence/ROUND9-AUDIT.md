# ROUND 9 AUDIT — branch `1.0.0-fix-9` (base `b579109c2a88b29405f80b80e1ebb254b825656b`)

## Counts and outcome

21 findings from four parallel read-only agents (backend, UX, tests, docs):

- backend: 5 (1 SHOULD, 4 LOW)
- UX: 4 (2 MUST, 1 SHOULD, 1 LOW)
- tests: 6
- docs: 6

All 21 were applied. No test was weakened; strict/BROKEN assertions kept.

System One gates for the audit round:

- overall `apply_all` PASS — `0.8972958624013666`
- artifact determinism PASS — `0.9653403660710159`
- backend PASS — `0.8591531846819046`
- **WEAK, disclose in final report**: UX `0.45917441966222555`, docs `0.29181342164170876`, tests `0.6606509444401095`

Overall/deterministic/backend PASS was the basis for applying all 21 despite the weak group verdicts.

## Backend (5)

| severity | file:line | issue | fix |
|---|---|---|---|
| SHOULD | `transport_unix.go:94` + `decision_bridge.go:1000-1012` | upstream `up` had no deadline; server read loop never refreshed it, so a silent upstream block kept `readFrame` alive and never released the `sem` slot (`transport_unix.go:52,64`) | set/refreshed `SetDeadline(time.Now().Add(httpReadTimeout))`, mirroring the client loop at `decision_bridge.go:968-970` |
| LOW ×4 | backend files | smaller correctness/robustness items (cache warm-hit refresh state, log-lock protection of clear, `/api/combo` method guard, exact cap) | fixed in place, no new abstraction |

## UX (4)

| severity | file:line | issue | fix |
|---|---|---|---|
| MUST | `web/index.html`, `web/style.css` | cold deep link left sheet iframe blank | hash load now initialises the iframe |
| MUST | `dashboard.go`, `web/style.css` | `CLEAR LOGS` destructive button had no danger affordance | `--danger` styling / `.btn.btn-danger` applied |
| SHOULD | `web/index.html`, `web/style.css` | modal focus escaped to background; `aria-modal` false | background made inert while sheet open |
| LOW | `web/style.css` | origin link overlapped sheet title on narrow viewports | mobile sheet offset adjusted |

Caveat: the origin URL remains visible while a sheet is open (required by the standing "URL always visible" request) but is non-focusable while the sheet has focus. No keyboard escape path was removed.

## Tests (6)

Fixes in `sim_e1_e2_test.go`, `sim_q3_q4_test.go`, `sim_q5_q6_test.go`, `checker_sim_test.go`, finalgate tests: resolve shipped binary with `filepath.Abs` and digest JSON instead of circular hardcoded SHA; assert SHA shape and whole-repo green; fix path/offset fixtures; validate report offsets as well as segment labels; keep `-buildvcs=false` so digest is independent of dirty/clean tree, with `go build` before `-race` in `scripts/verify.sh`.

## Docs (6)

`SURFACES.md` gained shipped LOGS/PROMPTS surfaces (`/api/logs/clear`, `/api/prompts/add`, tail-limited read, toast forwarding) and dropped the stale `.Demo` bullet (deleted in round 5, see `ROUND5-AUDIT.md:43`). `CHANGELOG.md` gained a 1.0.0-fix-9 entry. `RAPOR.md` cites the verified AES S-box offset `0x895440`. Finalgate claims K12/K13/K15/K16 refreshed with current evidence (see `scripts/finalgate/main.go:45-48`).

## Provider probe fix (the round-9 headline defect)

Reported symptom: the combo announced `WINNER ON ROUND 1: OpenRouter/typesafe/jev-1.14 | 400 Bad Request | key accepted | 208 ms` and the key test printed `TEST FORBIDDEN` although both providers were reachable and the credentials were fine.

Two independent root causes, both in the probe, not in the credentials:

1. Wrong request shape. The probes posted a chat body `{"messages":[{"role":"user","content":"ping"}]}`. Every System One endpoint rejects that shape with HTTP 400/422, because the wire contract is `{"model":..,"state":..,"questions":{..}}`. The request was never valid, so the answer said nothing about the key.
2. Wrong status mapping. The combo treated every reply below 500 that was not 401/403 as "key accepted", so a 400 became a winner on round 1. `/api/keytest` mapped every non-200/non-401 answer to `{"status":403,"result":"FORBIDDEN"}`, so a 400 was reported as `TEST FORBIDDEN`.

Fix (`combo.go`, `keytest.go`, `dashboard.go`, `web/index.html`):

- `soProbeBody(model)` in `keytest.go` builds the System One contract body; the credential still travels only in the headers, never in the body.
- `comboModel(s)` in `combo.go` asks for the full model id of the step (`typesafe/jev-1.14`, not `jev-1.14`) so a 404 for a missing model stays distinguishable from a rejected key. `ComboStep.Model` was added and `toComboSteps` fills it from the saved step.
- Truthful classification in `probeComboStep`: only 2xx means `key accepted`; 401/403 with a key means `key rejected`; 400/404/422/429/5xx fail over to the next step; without a key a non-5xx except 404 proves only `endpoint reachable`.
- Truthful classification in `keyTestResult(status)` (dashboard.go): 401 `INVALID KEY`, 403 `FORBIDDEN`, 404 `MODEL NOT FOUND`, 429 `RATE LIMITED`, 400/422 `BAD REQUEST`, no answer `ENDPOINT UNREACHABLE`, >=500 `SERVER ERROR`. `/api/keytest` now tries every stored key and stops only on an answer about the request itself (400/404/422/429/5xx).
- The dashboard advances to the next model on 403 or 404 and stops on 400, because a refused request would fail the same way for every model.

Tests added (no assertion weakened): `TestComboNon2xxWithKeyDoesNotWin`, `TestComboBadRequestWithKeyDoesNotWin`, `TestComboProbeBodyUsesStepModelField`, `TestKeyTestResultNamesTheStatus`, `TestKeyTestReportsBadRequestInsteadOfForbidden`.

## Additional live-SO wire fix (after the 21 findings)

Live sweep exposed two wire defects, both SO-approved and applied:

- response top-level `id` added to strict allowlist — SO `go 0.956846747321964`
- live endpoint rejected top-level `session_id`/`prompt_cache_key` with HTTP 422 `"session_id: fazladan alan gönderildi"`; stable hashes moved into `state` via `soWireState` in `gate.go:705-719` — SO `go 0.8991160943393134`. FA8 test (`sim_fa7_fa8_test.go`) rewritten to assert the hashes are present in `state`, bodies stay stable across volatile session fields, and no rejected top-level keys are sent. `sim_q3_q4_test.go` / `sim_q5_q6_test.go` updated to assert the new state prefix. No assertion weakened.

## Verification evidence

- full `CGO_ENABLED=0 go test -count=1 ./...` — EXIT 0, 7/7 packages
- `scripts/verify.sh` — EXIT 0, 10/10 PASS, verbatim in `scripts/finalgate/evidence/K12.txt`
- `scripts/relay56/TRANSCRIPT.txt` — 28 PASS, 0 FAIL, RELAY-OK
- `scripts/finalgate/evidence/K15.txt` — 3 local, 55 admitted, 52 refused, 55 low-conf steered, 52 low-confidence non-proceed, 3 with no non-proceed label
- shipped binary `71b993f22a92edfe5c9eb5d87c858dd71231365cdbb01c4640fd5ea374d170e2`, 11129 pclntab funcs, AES S-box `0x895440`
- `scripts/finalgate/evidence/K16.txt` — honest degraded live sweep: `0 admitted, 0 refused, 0 low-conf, 55 degraded, 3 local`, PASS in 10.02s. Direct probe answered HTTP 200 with a valid typed choice in 13.07s, above the shipped 5s enforcing deadline at `gate.go:34`. Degradation is endpoint latency; no timeout constant was raised to manufacture green evidence. Stub sweeps carry the routing proof.

## Honest limitations

- UX/docs/tests group SO verdicts were weak (values above); group-level grading is not strong evidence.
- Live SO enforcing evidence is degraded; `K16.txt` records it explicitly. It is not presented as healthy live proof.
- No force push, tag, main merge, deploy, release or dashboard/MCP restart was performed for round 9.
