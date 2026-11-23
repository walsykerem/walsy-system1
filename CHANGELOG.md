# Changelog

## 1.0.0-fix-7 (2026-11-23) - stable, M1 approval: pending (config fields set: approver=m16221, m1_approval_owner=m16221, so_go_confidence=0.8837; SO go 0.8837 / auth-m1-semantics 0.9393 / config gate 0.9083; no approval record, RELEASE_APPROVAL still required)

- Completed the 70-TODO wave work: single decision hub, method-bound scope, receipt hash chain, canonical `gateKey`, decision cache with singleflight, preflight shadow, breaker/budget guard, strict SO decode, build SHA stamping, cost metering, provenance `/api/health`, ranking trace, engine-generation guard, answer validation, admit-rate counters, N17 state skeleton (prompt bodies stay local).
- Test suite is hermetic: `TestMain` isolates `HOME` so a machine-level `so_mode=enforcing` cannot change test outcomes; tests needing specific state still override it.
- `scripts/verify.sh` defines 10 stages (gofmt, vet, `-race` tests, clean build, lrecon digest, stdlib ASCII scan, subpackage vet, decision_items signature, release gate, verdict). Full 10/10 run is pending the parent: stage 9 requires `RELEASE_APPROVAL` and a provider key, which subagents do not read.
- E1 enforcement enabled via `so_mode=enforcing` in `~/.walsy-system1/config.txt` (0600). Advisory fail-open retained for no-key/degraded paths. Loopback-only; no key material in any artifact.
