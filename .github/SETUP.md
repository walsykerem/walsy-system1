# CI setup controls (not automated by this PR)

This PR adds workflow YAML only. Every item below is a **repository/organization setting** and is NOT automated by this PR.

## Required in GitHub settings

- **Secret scanning + push protection**: enable for the repo, with a custom pattern matching the Evren API key format, so a token cannot be committed or pushed. CI holds no API key and makes no live System One call.
- **Branch protection on `0.1.0-fix-9`**: require passing status checks, require review, disallow force pushes and deletion.
- **Environment approval**: gate the SO-protected / provenance manual workflow behind an environment with required reviewers. Only an authorized human approves; the workflow holds no signing key and publishes nothing.
- **CODEOWNERS for `.github/workflows`**: add an owner so workflow edits require security review (add the CODEOWNERS file outside this PR).
- **Loopback-only self-hosted runner egress** (if a self-hosted runner is used): allow loopback egress only; no dashboard or SO endpoint reachable from CI. No workflow starts a service.

## Hermetic workflows

All 21 workflows in the suite are hermetic: no live System One stage runs in any of them. The four in this PR:

| Workflow | Runner | Purpose |
| --- | --- | --- |
| `mach-o-claim-fixture-check.yml` | `macos-14` | Rebuild digest matches both fixture claims; function-count literal present (native symbol count needs walsyrea/lrecon locally). |
| `offline-checker-sim-green.yml` | `ubuntu-latest` | `CHECKER_SIM_SKIP_LIVE=1` checker sim, tmp-only `GOCACHE`. |
| `signed-provenance-attestation-manual.yml` | `ubuntu-latest` | Dispatch-only; verifies human-supplied SHA after hermetic rebuild; prints manual instruction, no signing/publish. |
| `loopback-egress-assertion.yml` | `ubuntu-latest` | Offline checker sim plus static loopback / no-wildcard-bind / no-PR-target assertions. |

No `pull_request_target`, no `secrets:` references, no `scripts/verify.sh` stage 9, no live network and no key in CI. True signed provenance requires an org-level trusted signer configured by org admins.
