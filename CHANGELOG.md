# Changelog

## 1.0.0-fix-7 (2026-11-23) - stable, M1 approval: pending (config fields set: approver=m16221, m1_approval_owner=m16221, so_go_confidence=0.8837; SO go 0.8837 / auth-m1-semantics 0.9393 / config gate 0.9083; no approval record, RELEASE_APPROVAL still required)

- Completed the 70-TODO wave work: single decision hub, method-bound scope, receipt hash chain, canonical `gateKey`, decision cache with singleflight, preflight shadow, breaker/budget guard, strict SO decode, build SHA stamping, cost metering, provenance `/api/health`, ranking trace, engine-generation guard, answer validation, admit-rate counters, N17 state skeleton (prompt bodies stay local).
- Test suite is hermetic: `TestMain` isolates `HOME` so a machine-level `so_mode=enforcing` cannot change test outcomes; tests needing specific state still override it.
- `scripts/verify.sh` defines 10 stages (gofmt, vet, `-race` tests, clean build, lrecon digest, stdlib ASCII scan, subpackage vet, decision_items signature, release gate, verdict). The full 10/10 run is recorded verbatim in `scripts/finalgate/evidence/K12.txt`; stage 9 used `RELEASE_APPROVAL` and a provider key held by the parent.
- E1 enforcement enabled via `so_mode=enforcing` in `~/.walsy-system1/config.txt` (0600). Advisory fail-open retained for no-key/degraded paths. Loopback-only; no key material in any artifact.

## 1.0.0-fix-9 (2026-11-24) - M1 approval: pending (no approval record, RELEASE_APPROVAL still required)

21 audit findings closed (5 backend, 4 UX, 6 tests, 6 docs) plus an approved live-SO wire fix. Full list and weak group verdicts: `scripts/finalgate/evidence/ROUND9-AUDIT.md`.

- Backend: upstream deadline refreshed on the server read loop; exact 8 MiB cap, warm-hit refresh state, log-lock clearing and `/api/combo` method guard corrected.
- UX: cold deep-link initialises the sheet iframe; `CLEAR LOGS` has danger styling; background is inert while a sheet is open; mobile sheet offset corrected.
- Live SO: top-level `id` accepted in strict decode; stable session/cache hashes moved into `state` because the endpoint rejects top-level `session_id`/`prompt_cache_key` with HTTP 422. Rebuilt binary `c3369b39540a65b0e292d8860314b9b909b62657ca79137b0b24ce810f3d19a6` (11128 funcs, AES S-box `0x891440`); fixtures and digest refreshed.
- `SURFACES.md` now lists the shipped LOGS/PROMPTS surfaces (`/api/logs/clear`, `/api/prompts/add`, tail-limited audit read, sheet toast forwarding) and drops the stale `.Demo` note in favour of `ROUND5-AUDIT.md:43`.
- `RAPOR.md` cites the verified AES S-box offset `0x891440`; the simulation checker now validates report offsets as well as the segment label.
- Simulation tests resolve the shipped binary with `filepath.Abs("walsy-system1")` and take the SHA from `walsy-system1.digest.json` instead of hardcoding it; checker-sim and finalgate tests assert whole-repo green and digest SHA shape.
- CI (2026-11-24): 21 fork-safe workflows added under `.github/workflows/` with SHA-pinned actions and no persisted credentials, covering Go core, static build, data integrity, hermetic SO, finalgate determinism, evidence freshness, relay hashpin, claim drift, audit docs, Mach-O fixture, secret scanning, loopback egress, atomic bundle, protected SO dispatch, fork isolation and release-tag/CHANGELOG binding.
- The dashboard header no longer renders the `127.0.0.1:8787` origin link (`web/index.html`, `web/style.css`); the postMessage origin check remains. E2E fixtures updated accordingly.
- No main merge, deploy, release or dashboard/MCP restart was performed for round 9. `release.tag` now names `1.0.0-fix-9` solely for the workflow's branch-binding check; no release approval is implied. The binary was rebuilt locally; push needs its own SO `go` gate.
