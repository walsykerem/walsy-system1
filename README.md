# walsy-system1

## Usage

```
go run . -transport dashboard -listen 8787   # http://127.0.0.1:8787
```

`-listen` needs an explicit port (e.g. `8787`) for the dashboard. An empty
`-listen` is only meaningful for the other transports.

Default transport is `stdio`; existing clients are unchanged. Other transports:
`stdio`, `unix`, `http`. `-dashboard` is an alias for `-transport dashboard`.

The dashboard's BUILD TARGET panel cross-compiles a static `CGO_ENABLED=0`
binary for darwin/arm64, darwin/amd64, linux/amd64, linux/arm64, windows/amd64
or windows/arm64 into `dist/`. Until a target is saved, it uses this machine's
OS/architecture (native). MCP wiring is unchanged: the client launches this
binary directly, so the correct binary for that machine must be installed.

## MCP client wiring

A client must launch this binary as the local wrapper, or no traffic reaches the
gate. Print the OpenCode snippet with the upstream command as arguments:

```
walsy-system1 -print-client-config npx -y @example/mcp-server --flag
```

Paste the result under `mcp` in the client config. Shape:

```json
{"mcp":{"servers":{"system1":{"type":"local","command":["/abs/path/walsy-system1","-transport","stdio","npx","-y","@example/mcp-server","--flag"]}}}}
```

`command` starts with this binary (absolute path when available), then
`-transport stdio`, then the upstream command verbatim. The snippet contains
process wiring only — no API keys, tokens, or other secrets.

## System One mode

- `advisory` (default): fail-open. Every request is relayed byte-for-byte to the
  upstream with no added client-visible delay. No client change required.
- `enforcing`: explicit opt-in only, via `WALSY_SO_MODE=enforcing` or
  `so_mode=enforcing` in `~/.walsy-system1/config.txt` (env wins). On an SO deny,
  `tools/call` is stopped before the upstream and answered with a JSON-RPC error
  (never HTTP 503). It still fails open when SO is unavailable, returns
  low-confidence, or times out.

System One decision capabilities are on by default: `need_label`, `no_scratch`,
`safe`, `reversible`, `authority`, `calibrated`, and `phase=before-model`. There
is no dashboard SO CONFIG panel. A client frame can narrow the decision surface
with explicit `false` fields; nothing is inferred from raw content. No key
values or secrets are ever rendered.

## Dashboard API

- `POST /api/providers` with `{"name","base","keyEnv"}` — adds a provider. `base`
  must be a remote HTTPS URL or a loopback HTTP/HTTPS URL. Persisted to
  `~/.walsy-system1/providers.txt`, mode `0600`, as newline-separated
  `name,base,keyEnv` lines; adding again appends/updates instead of overwriting.
  A line whose name matches a built-in provider overrides its endpoint; last
  duplicate line wins. `keyEnv` is an env var *name*, never the secret value.
- `POST /ui/providers` — same fields as form data (`name`, `base`, `keyEnv`).
- `POST /ui/providers/endpoint` form `name`, `base`, optional `keyEnv` — saves or
  updates one endpoint. Missing `keyEnv` inherits the current/built-in value or
  defaults to `API_KEY`. Rejects invalid bases; no raw URL is echoed on error.
- `POST /ui/providers/reset` form `name` — removes that registry line so a
  built-in default returns (a custom provider with no default leaves the list;
  its stored keys/models return if it is re-added).
- `GET /ui/providers/sheet?name=<provider>` — minimal same-origin iframe
  document for one provider sheet. Every sheet form redirects back here with a
  `msg`, so the home page never reloads.
- `GET /ui/combo/sheet` — minimal iframe document with the RUN COMBO form.
  `POST /ui/combo/run` renders the ordered probe result inside that iframe; the
  home page never navigates and there is no standalone result page.
All nine built-in providers ship a documented default endpoint (OpenRouter,
Ollama Cloud, OpenCode Zen, System One variants, Nace Drex, Ollama Local,
Cloudflare Clef). Cloudflare Clef is the Workers AI model `@cf/cloudflare/clef`
(https://developers.cloudflare.com/workers-ai/models/clef/): it takes the same
`state`/`questions` body, has no dedicated SDK (Workers AI binding `env.AI.run`
or plain REST), and its template contains `YOUR_ACCOUNT_ID`; replace it in
the sheet. Every non-demo sheet has a SYSTEM ONE CONFIG block: default
endpoint, GET API KEY link, editable BASE URL with SAVE ENDPOINT and a
conditional RESET.
Each sheet has a KEY TEST block: TEST sits left of REMOVE for every stored key
and probes the configured endpoint with coarse results only. Feedback from all
sheet forms renders inside the open sheet. While a sheet is open the page scroll
locks and only the sheet body scrolls. No key values are rendered.

- `POST /ui/build-target` form `target=<id>` — saves the selected build target.
- `POST /ui/build` — builds the selected target into `dist/`; the result is
  rendered in the BUILD TARGET panel.
- `POST /api/combo/run` with `{"names":[...]}` — probes the named providers in
  order; first reachable endpoint wins. A 2xx or a non-404 4xx (including 405
  Method Not Allowed) counts as reachable; 5xx and 404 fail and fall through to
  the next. No 503 is returned. The dashboard's combo sheet runs the same probe
  inside the home page; the parent page never navigates.

The `DEMO/OK`, `DEMO/DOWN`, `DEMO/SLOW` chips are view-model only — they are not
written into the provider registry.
