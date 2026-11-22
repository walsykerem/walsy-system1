# GAP CLOSURE — branch 1.0.0-fix-5 (base 9bc4103)

The five gaps documented at the end of round 4 (G1–G5) were re-decided by a
System One severity table (m13749) that asked, per gap, MUST vs SHOULD vs
not-a-gap. Every verdict came back below 0.8 (weak) and was retried once under a
tighter state; the labels were stable across the two passes, so the decision was
taken on them with the weakness stated here.

| gap | SO severity verdict (weak, retried once) | decision | closure |
|-----|------------------------------------------|----------|---------|
| G1 model generic-no passes a tool call through | not-a-gap 0.248 / should | close as by design | K22 |
| G2 retry budget refills without bound | should 0.544 | fix | K20 |
| G3 `samePath` mixed read/write | must 0.563 | fix | K18 |
| G4 destructive shell command not judged | must 0.537 | fix | K19 |
| G5 three unbounded inputs | should 0.422 / low | fix | K21,K23,K24 |

## What changed

- **G3** `write_guard.go:73` `samePath` is now directional in the mixed case: a
  pathed read (`/repo/sub/a.go`) still licenses the same file named bare
  (`a.go`), but a bare read never licenses a pathed write. Test
  `TestWriteGuardBareReadDoesNotLicensePathedWrite`; the earlier
  `TestWriteGuardMatchesRelativeToAbsolute` still passes. Claim K18 0.8334.
- **G4** `diff_guard.go:38` `destructiveCommand` matches `rm -rf`, `mkfs`,
  `dd of=`, `shred`, `truncate -s 0` only on a command tool
  (`argsDestructiveCheck` :93, `argsGuardCheck` :102), so an ordinary write whose
  body merely mentions `rm` is not refused; the refusal is read back live from
  the audit log (`TRANSCRIPT.txt:20-21`, reason `diff-dangerous-command`). Test
  `TestArgsDestructiveCheck`. Claim K19 0.8211.
- **G2** `router_policy.go:115` `routerMaxRefills = 8`; `routerBudgetReset`
  (:118) stops refilling once the counter reaches it, so looping `initialize`
  cannot refill forever. Test `TestRouterBudgetRefillsAreBounded`. Claim K20 0.8824.
- **G5** `transport_unix.go:41` `unixMaxConns = 64` bounds concurrent served
  connections (semaphore :47, `TestServeUnixConcurrencyCap`); `mcp_tools.go:14`
  `maxToolsFrame` bounds one stdin frame via `frameLimitReader` (:26) returning
  `errFrameTooLarge` (`TestToolsServerRejectsOversizedFrame`); `transport_http.go:143`
  `requestID` echoes the caller's id on an upstream failure instead of `id:null`
  (`TestRequestIDReadsFrameID`, `TestProxyHTTPFailureEchoesID`). Claims K21
  0.8404, K23 0.8106, K24 0.8916.
- **G1** left as designed behaviour: the model's generic `no` still forwards
  (`TRANSCRIPT.txt:22`), and only deterministic refusals block (unread-write,
  diff-secret, diff-dangerous-command, budget). The model is advisory and never
  the sole gate. Claim K22 0.8494.

`var gaps` in `scripts/finalgate/main.go` is now empty. All 24 claims are `done`
at ≥0.8; see `FINALGATE-R5.txt` (AGGREGATE 0.8597 over 24, min 0.8106,
VERDICT COMPLETE 24/24) and `K12.txt` (verify.sh PASS 8/8).
