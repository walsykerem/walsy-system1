# Verification template (Agent 4)

Parent: run `bash scripts/verify.sh` after all parallel agents finish. Do not run early.

## 7 gaps under test (`zz_acceptance_test.go`)

1. Client wiring — `opencodeMCPSnippet("walsy-system1", []string{"stdio","echo"})` contains `walsy-system1`, `-transport`, `stdio`; valid JSON.
2. Mode default — `soMode()` is advisory by default, returns advisory/enforcing only.
3. Transport flags — `-upstream` and `-print-client-config` registered (`flag.Lookup`).
4. Capability opt-in — `soCapability()` empty by default; all fields true, `phase=before-model` with explicit `~/.walsy-system1/config.txt`.
5. HTTP security — POST with wrong `Origin` or mismatched `Mcp-Method` to `mcpHTTPHandler()` returns non-200.
6. Smart router — `routerAllow`, `routerBudgetOK`, `routerRecord`, `routerAudit`, `routerMirrorURL`, and weighted `RunCombo` all work.
7. Per-request gating — `newDecisionBridge().decide()` succeeds for two distinct initialize frames; no one-shot latch.

## Exact commands `scripts/verify.sh` runs, in order

1. `gofmt -l *.go scripts/*/*.go` — must output nothing
2. `CGO_ENABLED=0 go vet ./...`
3. `CGO_ENABLED=1 go test -race -count=1 ./...` — race detector needs cgo
4. `CGO_ENABLED=0 go build -trimpath -buildvcs=true -o walsy-system1 .`
5. `lrecon --json ./walsy-system1` consumed by `go run ./scripts/recondigest` — digest + kind only
6. `go run ./scripts/asciiscan *.go web/*` — stdlib non-ASCII scan, no `python3` in product verification
7. `CGO_ENABLED=0 go vet ./scripts/...`
8. `shasum -a 256 -c decision_items.json.sha256.sig`
9. `go run ./scripts/releasegate -so-final-complete 0.8` — needs `RELEASE_APPROVAL` and a provider key; subagents must not run it
10. `echo PASS` — only if all above succeeded

## Records to fill after the full run

- lrecon digest: historical `3568a33005c91a781f1575ec379942c0ff72369eb9a528f481164b5865d24d6d` (fix1), `30cb4b0cbff699d4803c12d67b0e5a550958ee7b01df5588c936cf35f4dff4dd` (fix2); current (post-fix3) `edad166da066b1881efd2489a80c5c5f8e5171c0df3efad7c9b36da10f3cdc4a`
- agent-lsp approval: `start_lsp OK`, `blast_radius` on 19 changed Go files, `get_diagnostics` 19 files, 0 severity-1 errors
- UI check: `GET /` 200, BUILD TARGET panel with 6 platforms + NATIVE, no `#so-config` panel, no `/ui/so` form, no key values; `GET /style.css` `--bg: #E5E5E5`, no `<script>`/CDN/font/node_modules refs. SO capabilities are on by default (`soCapability()`), advisory/fail-open mode unchanged. browse MCP paired-offline; verified by `TestJSParityNoSOConfigPanel`. Auto-on reached by `TestSOAutoOnForPlainClientFrame` (plain `initialize` frame triggers SO with built-in cap fields).
- client config snippet: `-print-client-config` merges into a temp `opencode.json` copy, valid JSON, `type=local`, `command[1:3]=[-transport stdio]`, binary path exists
- enforcing end-to-end: covered by passing `TestEnforcingDenyBlocksToolCall`, `TestEnforcingOutageFailsOpen`, `TestPerRequestGating`
- K16 live sweep (same command finalgate cites): `WALSY_LIVE=1 go test -run TestEveryDecisionPointLive -count=1 -v .` (needs a provider key)
- live SO via Evren: `choice=yes` conf bucket `0.8`, key never printed
- final gate verdict: `complete`, confidence `0.8166174393048045`, p(complete)=`0.8777449595365363` (headless render + live binary e2e closed the weak-verdict gaps)

Placeholders stay unfilled until the parent runs the suite.

## UI / provider-sheet rework (m04019)

- `bash scripts/verify.sh` PASS: gofmt clean; `CGO_ENABLED=0 go test -count=1 ./...`
  `ok walsy-system1 3.321s`; static build clean; lrecon kind `macho`, digest
  `da95a88d9184cdee6d7d481c68d3c53ebd87cb32183a38b6f921b7c150fef880`;
  ascii clean 42 files. agent-lsp `get_diagnostics` reported no errors.
- ux_lint on `web/`: score 95 (inline-style finding gone); only
  `imagery-mandatory-missing` high at `web/index.html:1` — formally waived
  because images are forbidden by project constraints.
- Live dashboard (`127.0.0.1:8787`, new binary): `GET /` 200; two ADD PROVIDER
  posts both 303 `PROVIDER SAVED`, registry held 3 lines, API listed both new
  providers (15 total); `POST /ui/providers/endpoint` 303 `ENDPOINT SAVED` and
  changed the target base; `POST /ui/providers/reset` 303 `ENDPOINT RESET` and
  restored the built-in default. Test registry state was restored to its
  original single line, mode `0600`, after the checks.
- Rendered home page: 11 provider sheets with SYSTEM ONE CONFIG / GET API KEY /
  SAVE ENDPOINT; zero `<script>` tags. CSS locks page scroll via
  `body:has(.sheet-backdrop:target){overflow:hidden}` and scrolls only
  `.sheet-body` (`overscroll-behavior:contain`).
- browse MCP still `paired-offline` at verification time; live DOM click-through
  pending until the paired browser profile with the extension is opened.
- Real-browser visual check via headless Google Chrome (`--headless=new`,
  `--screenshot`, 1440x1100 / 1440x900): home page shows ORDERED COMBO with
  BUILD TARGET immediately below it in the right column; Evren sheet shows
  SYSTEM ONE CONFIG (DEFAULT ENDPOINT, GET API KEY, notice, BASE URL,
  SAVE ENDPOINT, RESET) and no key value. A worst-case sheet with the maximum
  `maxKeysPerProvider = 8` keys stayed inside the viewport with internal
  overflow; the page behind did not move. All 9 form actions map to real routes
  (23 submit buttons). Screenshots:
  `/private/var/folders/_h/qvftqp9j125dppqpdv8kyb8m0000gn/T/opencode/walsy-main.png`,
  `walsy-sheet.png`, `walsy-scroll12.png`. Temporary probe provider/keys were
  removed and the registry restored to its original single `0600` line.

## Sheet iframes + combo POST probe (m04822/m04838)

- `bash scripts/verify.sh` PASS: gofmt clean; `CGO_ENABLED=0 go test -count=1 ./...`
  `ok walsy-system1 3.433s`; static build; lrecon digest
  `4057a58b7ccfcac20edd1afc63ac0442c0ac456ceb4963b77b0e2ea1ddc73d55`, kind
  `macho`; ascii clean 45 files.
- ux_lint on `web/`: score `100`, 2 files, 154 rules, 0 findings.
- Live dashboard (`127.0.0.1:8787`, new binary): `GET /` 200 with provider and
  combo iframe `src` values and zero `/ui/combo/run` occurrences in the home
  document. `GET /ui/providers/sheet?name=Evren+System+One` has SAVE ENDPOINT and
  ADD API KEY, no topbar, no `<script>`, and no RESET/CLEAR ALL KEYS with an
  empty key store. `GET /ui/combo/sheet` contains `action="/ui/combo/run"` only.
  `POST /ui/combo/run` (same origin) returned 200 with
  `WINNER: Evren System One/cloudflare/clef`, proving a 405 response now counts
  as reachable; the result document has no BACK link and no topbar.
- Sheet redirect: invalid endpoint POST returned 303
  `Location: /ui/providers/sheet?name=Evren+System+One&msg=ENDPOINT+REJECTED`,
  so only the iframe navigates and the parent page never reloads.
- browse MCP still `paired-offline`; no click-through. No commit/push.
- Enforcing-mode write guard (`write_guard.go`): live stdio replay blocked a
  write to a never-touched path with `-32001 blocked: write to a path this
  session never touched: /tmp/never-read.txt`, forwarded the same call after a
  `read_file`, and blocked the 9th identical `tools/call` as a repeat. Advisory
  mode (default) forwarded all three unchanged. `verify.sh` PASS, ascii clean
  59 files.
- Every registry item now carries a `requirement` line (41/41), asserted by
  `TestEveryRegistryItemStatesItsRequirement`. The 24 lines that had no
  user-message trace are derived from each item's own rubric, not quoted from
  the user; confirming them is still open.
