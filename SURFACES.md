# Dashboard surfaces — audit checklist

Every clickable spot, sheet and feature in the local dashboard, numbered so each
one can be approved or failed on its own. Live server: `http://127.0.0.1:8787/`.

Status key: PASS (works, verified) · GAP (wrong, fix needed) · UNVERIFIED.
Verified by four read-only agents (A · B+C · D+E · G, source + httptest) and one
real-browser agent (F, Chrome on an isolated :8794 instance), then re-checked by
`go test ./...` and curl against the live server.

## A — Providers (home rows + add form + provider sheet)

- A1 row link `#sheet-<name>` opens its sheet — PASS (all rows, iframe title matches; F)
- A2 row `X` → `POST /ui/providers/remove` — PASS (custom row gone; baked rows render no X and a crafted POST answers `BUILT IN PROVIDER`)
- A3 `+ ADD PROVIDER` `<details>` — PASS (no layout shift; BUILD TARGET top stayed 293px)
- A4 `SAVE PROVIDER` → `POST /ui/providers` — PASS (`PROVIDER SAVED`)
- A5 sheet `SAVE ENDPOINT` → `POST /ui/providers/endpoint` — PASS (bad base → in-sheet `ENDPOINT REJECTED: …`, no silent snap-back; success now shows a home-page toast, see fix 2)
- A6 sheet `RESET` → `POST /ui/providers/reset` — PASS (only offered when base diverges; restores the baked base; custom provider with no baked base → `NO DEFAULT ENDPOINT TO RESET`, key file kept)
- A7 `ADD MODEL` and model pill `x` → `POST /ui/providers/models` — PASS
- A8 sheet `X` and backdrop — PASS
- A9 `Escape` — PASS (closes without reloading; typed form values and the open `<details>` survive)
- A10 `GET /ui/providers/sheet?name=` — PASS (unknown name → 303 `INVALID PROVIDER`)
- A11 `POST /api/providers/sheet` (JSON) — PASS (nil `Models` preserves the list)
- A12 `GET /api/providers` — PASS (`no-store`)

## B — Keys (provider sheet)

- B1 `TEST` — PASS (`KEY TEST 401 UNAUTHORIZED` / `KEY OK`; never prints the value)
- B2 `REMOVE` — PASS
- B3 `ADD KEY` — PASS (duplicate → `KEY ALREADY SAVED`, 8-key cap → `KEY LIMIT REACHED`, unknown provider → `INVALID PROVIDER`, no orphan file)
- B4 `CLEAR ALL KEYS` — PASS (idempotent)
  (all `POST /ui/providers/keys` with the `action` field)

## C — Combo

- C1 `RUN COMBO` → `POST /ui/combo/run` — PASS (`WINNER ON ROUND n`, `NO WINNER AFTER n ROUNDS`, 401/403 → `key rejected` fail-over)
- C2 `+ ADD TO COMBO` → `GET /ui/combo/add` — PASS
- C3 combo add sheet model buttons — PASS
- C4 `SEARCH` button — PASS (server-side, works without JS)
- C5 live search filter while typing — PASS (`input` listener hides non-matching `.model-group`; a provider-name match keeps its whole group)
- C6 model select → `POST /ui/combo/add` — PASS
- C7 `DONE` link — PASS
- C8 ordered list `UP` / `DOWN` / `X` → `POST /ui/combo` — PASS (first/last disabled, invalid index refused)
- C9 `GET /ui/combo/sheet`, `GET /ui/combo/add/sheet` — PASS
- C10 `GET /api/combo` — PASS
- C11 `POST /api/combo` — PASS · C12 `POST /api/combo/run` — PASS

## D — Build

- D1 six target buttons → `POST /ui/build-target` — PASS (selection readable from `TARGET:` line only, no `aria-pressed`)
- D2 `NATIVE` → `POST /ui/build-target` — PASS (`darwin/arm64`)
- D3 `BUILD SELECTED TARGET` → `POST /ui/build` — PASS (`BUILD OK: dist/…`, concurrent → `BUILD ALREADY RUNNING`)
- D4 build result page and `dist/` output — PASS (`dist/` gitignored)

## E — Infrastructure / global

- E1 `GET /` — PASS · E2 `GET /style.css` — PASS · E3 `GET /favicon.ico` — PASS (204) · E4 `GET /api/health` — PASS
- E5 unknown route — PASS (404 JSON)
- E6 method and `sameOrigin` guards — PASS (405 on wrong method, 403 cross-origin, 400 malformed)
- E7 loopback `Host` guard — PASS
- E8 `Cache-Control: no-store` — PASS (documents, assets, JSON)
- E9 hash navigation — PASS (back button restores closed state; combo POSTs return to `/` without the hash, accepted)
- E10 MCP `-transport tools` — PASS (`system1_status`, `system1_audit`)

## F — Logs and prompts (shipped; source-checked, not live-verified in round 9)

- F1 `POST /api/logs/clear` — shipped (`dashboard.go:641`): same-origin POST
  clears the in-memory ring and `audit.jsonl`, returns `{"ok":true}`; the LOGS
  sheet's `CLEAR LOGS` button confirms first.
- F2 `POST /api/prompts/add` — shipped (`dashboard.go:711`): same-origin JSON
  `{name,text}` with both fields capped, returns `{"ok":true}`; `ADD PROMPT` in
  the PROMPTS sheet keeps its plain POST fallback.
- F3 LOGS sheet tail read (`logs.go:240,332`) — at most `logRingSize` newest
  persisted rows from the last 1 MiB; a fresh load falls back to audit rows.
- F4 Sheet toast forwarding (`logs.go:329-335`) — the redirect's `msg`/`MsgMs`
  render as a timed toast inside the LOGS iframe; `LOGS CLEARED` is `toast-ok`.

## Fixes applied after the audits

1. Success and failure toasts render with distinct classes (`toast-ok` / `toast-error`).
2. A sheet's success message is now forwarded with the parent refresh (`walsy-refresh|<msg>`), so `KEY ADDED`, `MODEL ADDED`, `ENDPOINT SAVED` show on the home page instead of dying with the iframe; refusals and key tests still stay inside the sheet.
3. `Escape` sets `#providers` instead of reloading the document.
4. Add Model filters while typing (one `input` listener; duplicate copies of that listener and of the provider-match logic removed).
5. Dead `POST /ui/providers/sheet` route removed; JSON errors keep their status; `no-store` on JSON.
6. Focus moves to the open sheet's close control (`aria-modal` on all three dialogs, duplicate `.sheet-note` rule and dead CSS removed).
7. Dead provider `Demo`/`Message` plumbing removed (`ROUND5-AUDIT.md:43`, B1); no test asserts those fields.

Re-checked in a real Chrome on the rebuilt binary (throwaway HOME, isolated port):
C5 filters while typing with focus kept and no reload; A9 Escape keeps typed values
and `<details>` with hash `#providers` and no reload; A5 SAVE/RESET and B3 ADD/REMOVE
show `toast-ok` on the home page with the sheet still open and rows already updated;
a rejected base still answers in-sheet `toast-error`. All PASS.

## Accepted, not changed

- Build target buttons carry no `aria-pressed`; the `TARGET:` line states the choice.
- Combo writes redirect to `/` and drop the hash.
