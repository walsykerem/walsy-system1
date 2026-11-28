# walsy-system1 — static map; advisory, fail-open by default

Scope: read-only. One binary artifact plus its Go source tree. Static evidence only;
No live SO call was made while producing this report.

## Binary identity

- Artifact: `walsy-system1` (darwin/arm64, `CGO_ENABLED=0`). The sha256 and size are recorded in the repo-root `walsy-system1.digest.json`; the full digest is kept there and not reproduced in this map.
- Build metadata names the supervising process with `vcs.modified=true`.

| ID | Area | Static finding |
| --- | --- | --- |
| 20 | Supervising process | KeepAlive+RunAtLoad, -dashboard -listen 8787 |
| 21 | Config parsing | empty so_mode fails open to advisory |
| 22 | Mach-O constants | aes_sbox at 0x8ad480 lives in __DATA,__noptrdata |
| 23 | Procedures | procedures are pclntab names only; Go names recovered from __gopclntab |

## Decision path (single surviving diagram)

```text
[transport: stdio|unix|http|dashboard]
        │
        ▼
   decodeFrame
        │
        ├─ ADVISORY ─ upstream (byte-for-byte, fail-open)
        │
        └─ ENFORCING ─ SO endpoint ─ upstream or -32001
```

Default mode is advisory: a denied or unreachable oracle never blocks the client.
Enforcing is explicit opt-in only.

## Release status

`release.tag` is `0.1.0-fix-7` only. M1 approval is pending: no approval record
exists and the release gate still requires `RELEASE_APPROVAL`. No approval is
claimed by this report.
