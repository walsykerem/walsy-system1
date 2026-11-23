# Changelog

## 1.0.0-fix-7 (2026-11-23)

- Completed the 70-TODO wave work: single decision hub, method-bound scope, receipt hash chain, canonical `gateKey`, decision cache with singleflight, preflight shadow, breaker/budget guard, strict SO decode, build SHA stamping, cost metering, provenance `/api/health`, ranking trace, engine-generation guard, answer validation, admit-rate counters, N17 state skeleton (prompt bodies stay local).
- Test suite is hermetic: `TestMain` isolates `HOME` so a machine-level `so_mode=enforcing` cannot change test outcomes; tests needing specific state still override it.
- `scripts/verify.sh` 8/8 PASS (gofmt, vet, `-race` tests, clean build, lrecon digest, ASCII scan, subpackage vet, verdict).
- E1 enforcement enabled via `so_mode=enforcing` in `~/.walsy-system1/config.txt` (0600). Advisory fail-open retained for no-key/degraded paths. Loopback-only; no key material in any artifact.
