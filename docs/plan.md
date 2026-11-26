# System One router - Go daemon (`routerd`) plan

## TO-DOs - single checklist (System One approved; G = m00559 survival; H = m01041 router usage)

> This list is a summary of the whole plan. In implementation **no item is skipped**; each item is detailed in a section below. `SKIP` items are deliberately not done - they do not come back.

**A. Core / architecture (§0, §1d)**
1. Single static Go binary: `CGO_ENABLED=0`, stdlib only, no external deps. No config file -> `os.Getenv("SYSTEMONE_ROUTE_URL")`, `os.Getenv("NINE_ROUTER_LOG")`. Distribution via `go install`/brew/single download; no client-specific code, only the address changes.
2. Single JSON-RPC 2.0 relay core (`mcp_relay.go`); **v1: only `transport_stdio.go`**. On day 3: `transport_unix.go` + `transport_http.go` (Streamable HTTP, `MCP-Protocol-Version` in one place); no SSE.
3. Protocol fidelity: `id`, `_meta.progressToken`, `protocolVersion` copied verbatim; version taken from the client (no hardcoding); `sampling`/`roots`/`elicitation` passed through uninterpreted; **unknown capability -> pass byte-for-byte uninterpreted; only security/scope decisions are deterministically rejected (no silent swallowing means: a security decision never passes silently)**. Rationale: SEP-2577 roots/sampling/logging deprecated (confirmed by go-sdk README); 3am on-call: telling an unknown capability "desteklemiyorum" [EN: "I do not support it"] breaks the client.
4. `capabilities.experimental['com.kerem/systemone-route'] = {enabled:false, mode:"relay"}` is advertised at initialize (`schema.ts:310-314,386-390`, `additionalProperties:true`; no top-level `x-`). SDK default-off -> clients that opt in get routing, those that do not get transparent relay. Output = **coverage ratio + rejection count**, NOT a guarantee.
5. Route order: Layer 0 deterministic (normalized hash + static short-circuit, ~us) -> Layer 1 System One (only AGENTS.md label-shaped decisions, `conf>=0.8`) -> Layer 2 transparent passthrough (default).
6. Fail-safe: `conf<0.8` / timeout / HTTP!=200 -> transparent passthrough. Fail-closed (503) is **not the default**. First 3 days in shadow mode (call + log the decision, passthrough not broken).
7. `before_model` is absent from the MCP spec -> "her sistemde model-öncesi garanti" [EN: "pre-model guarantee on every system"] is impossible. Instead of claiming a guarantee, report the coverage ratio; product positioning: "görünür kapsam ajanı" [EN: "visible coverage agent"].

**B. Provider registry / ordering / port (§1e)**
8. Registry: compile-time `.rodata` slice of 9 (OpenRouter, Ollama Cloud, OpenCode Free, v1m System One, Evren System One, Nace Drex, Ollama Local, Cloudflare, OpenCode Zen) + one-line `providers.txt` (#10 slot: `name, base URL, key env name`). **Keys are never written to the file**, they come from env. Order = file line order; if no file, baked 9 order.
9. `providers.txt` trust boundary: `0600` + fixed path + **symlink rejection** (mandatory trust boundary). Key env name is not visible in the file. Endpoint validation: **loopback Host check only** (non-loopback Host -> passthrough; if needed for DNS rebind protection). Secrets are never logged.
10. Ordering: **sticky pin** - last successful provider ID as a single string in process RAM (disk I/O is YAGNI). No pin after restart -> config order; first success writes the RAM pin. Request uses the pin; on first error drop the pin and switch to `hash(sorted(reachable IDs))` order. No clock/latency/health-probe; no scoring; no System One on every call (SO 0.937).
11. Custom endpoint for System One models (v1m/Evren) = `SYSTEMONE_ROUTE_URL` env. If set, route there; otherwise normal provider. No separate mutable layer / negotiation.
12. Port fallback: stdio/unix socket has no TCP port at all (collision impossible). HTTP mode `127.0.0.1:0` -> OS picks a free ephemeral port; `listen_addr` is already written to the mandatory stderr metric line. Fixed-port override via `MCP_PROXY_PORT` env is optional. **No port++ retry, no SO_REUSEPORT, no separate discovery file** (weak/contradictory SO vote on ports -> evidence + YAGNI decision).

**C. Cache / decision cost - MAIN GOAL (§2, §9)**
13. **Decision cache EXISTS - main goal (pivot m00752/SO approval 0.980, §9):** 16 ADHD branches + GPTCache/LiteLLM/ollama sources. **Store SO's typed decision, not the answer** (label + conf bucket + rubric version) - low entropy, paraphrases collapse into one bucket. **Key = canonicalize(state) + rubricVersion + model + conf>=0.8 rule + registry version.** `state` **is included** in the key, but canonicalized (`encoding/json` sorted-key copy + stripping volatile fields: timestamp/message-ref/tool-output/client name/session ID). **Rubric/template alone is not enough** - the SO label depends on state, rubric-only replay returns wrong decisions (deepen 1+3, saboteur `state tokenizer-spill` / `summary laundering`); the root cause of the old 0-hit result was *raw* state in the key, canonicalization fixes it. When rubric/deploy changes, old cells become deterministic **miss** (no invalidation code). **Negative cache:** `no`/`other`/`conf<0.8` are also written -> repeated ambiguous input skips the 1500ms SO call. **Store only the typed label, regenerate prose on every hit** -> stale summaries can never be served. **Admission threshold:** `conf>=0.8` AND probability margin >= threshold (clef is overconfident 0.97-1.0 -> a single wrong label must not spread to all clients). Hit = pure local read; no SO call. **In-flight coalescing on miss (m00894/SO 0.888):** ~15 lines of stdlib `Group{mu, m map[string]*call}` at the SO call site - 10 concurrent requests with the same canonical key share one 1500ms flight; `val`/`err` read only after `wg.Wait()`, cleanup uses `if m[key]==c delete(...)` identity check, leader panic->recover->passthrough, one caller's `ctx.Done()` cannot poison the leader, stale leader past 1500ms gets `Forget(key)`. **NO NEW MODULE**, including `golang.org/x/sync` (CGO=0 stdlib).
14. **Decision evidence:** no oracle -> the claim "hit arttı" [EN: "hits increased"] is measured with counterfactual replay (same corpus with cache on/off) + per-phase counters (pre/lookup/admit/llm_save); answer accuracy is not claimed. Every hit trace: `{key, decision_source, registry_version, conf_bucket, digest}` - **no raw conf, only a coarse bucket** (0.8/0.9/1.0; consistent with item 19/27 allowlist, offline calibration is done with buckets). Never secrets/PII.
15. **Canary + self-heal:** fixed questions with known labels are replayed every N calls; a wrongly cached canary -> cell flush + internal counter (no pager). 1/N hits are re-asked to SO in shadow mode and on mismatch -> cell flush (drift is evidence-based, not time-based).
16. **3-day shadow:** only key + hit/miss + margin are logged side by side, answers unchanged. Cache service is not enabled until hit candidates are measured. (No separate disk store / mmap / Redis - process-lifetime LRU `container/list`+map; restart->cold start is accepted.)

**D. Security / observability / health (§3, §4)**
17. System One call: `POST https://evren-llmapi.ssyz.org.tr/v1/systemone`, `model:"clef"`, header `x-api-key` from env. `questions` is an object (not an array), only `choice`/`score`. `score` is a 0..1 float + `legend` + index-keyed `probabilities`. Timeout 1500ms, **no retry**, `conf<0.8` -> None/fail-safe. `state` contains everything but never secrets/PII/long context; untrusted content goes in a separate field. The `localhost:20128/...` variant is not used.
18. Tool selection + parameter validation: allowlist/scope **first**, deterministic; JSON Schema ~20 lines (type/required/enum/properties/items); single candidate directly, multiple candidates + unresolved -> System One `choice`. **System One never makes permission/scope decisions.** No tool output / raw user text enters `state`. `conf>=0.8 && argmax`, one retry (6s), then deny/ask.
19. Observability (`metric.go`): **1 JSON line** to stderr, allowlist `tool/route/miss_reason/decision/model + ts/dur_ms + key_hash(12 hex) + listen_addr + seq/pid + reason_code/covered/abstained/passthrough + conf_bucket(0.8/0.9/1.0)`. **Raw** `conf`/`tokens`/`cost` are **outside the allowlist** - unverifiable, self-reported by our own process, no oracle in shadow; only coarse `conf_bucket` is logged (item 14/29 offline calibration). **Forbidden:** args, prompt, context, header, env, raw key, secret, raw conf. `jq -s` report; report the `gate_called` rate and coverage ratio + rejection count.
20. Health/fallback (`guard.go`): `classify(status,exc)` transient `408/429/500/502/503/504`+timeout, permanent `400/401/403/404/422` - KEPT for passthrough reason codes. Single bool `so_degraded`: once timeout/HTTP!=200 -> passthrough, no retry. Delete breaker / state machine / `health.json` (file I/O is YAGNI). If the backup is the same :20128, failover is meaningless (correlated failure).

**E. Test / implementation order (§6)**
21. Order: shadow mode (3 days) -> **v1 stdio** -> MCP relay test -> provider test -> `CGO_ENABLED=0 go build ./... && go test ./...`. On day 3: uniqueness measurement + unix + Streamable HTTP (no SSE).
22. `mcp_relay_test.go`: a client without extension completes `initialize` + tool call; a client with extension sees the `com.kerem/systemone-route` marker; `id`/`progressToken`/`protocolVersion` preserved.
23. `provider_test.go` (7 items): (a) if `providers.txt` is absent, 9 baked order, (b) single line -> #10, (c) no pin after restart -> config order, (d) key env name not visible in the file, (e) symlink rejection, (f) deterministic rotation on first error, (g) HTTP `listen_addr` loopback + port>0 + no secret in stderr.
24. Assert-based self-check for every new non-trivial logic; all tests, including `route_test.go`, must pass in `go test ./...`.

**F. SKIP - deliberately NOT DONE (does not come back)**
25. New HITL code (walsy-reflex v0.4.0 is already 80%; only 4 hard categories added to `policy.yaml`), session memory, rate limit/quota, shared multi-provider `generate()` interface, separate pre-filter `.mjs` file.
26. 9router patch / `semanticCacheEntries` layer, self-spawn (SO 0.880), network gate/DNS+307 (SO 0.008), LD_PRELOAD/eBPF, embedding/vector DB, new MCP server, fork of provider/cache layer, all `.py`/Rust paths, lrecon (PARK, §7). Survival (m00559): self-restart, global lock, cyst/epoch persistence, post-torpor cache-decision automation, quorum/external signal, new daemon, synchronous disk receipt. All reduce to item 6's single invariant - no separate code.

**G. Evidence (§8)**
27. **Single schema - no separate receipt FILE:** v1 live = one reason-code line on stderr. 3-day shadow = stderr append-only JSON line `{seq,pid,ts,decision,reason_code,covered,abstained,passthrough,latency_ms}`. `seq` gap = `pid` for distinguishing drop/restart (no separate file). Negative control: during the 3-day shadow, a deliberately malformed request must be rejected; if not rejected, the round is invalid. No line = not gated. **Raw** `conf`/`tokens`/`cost` unverifiable (self-reported by our own process, no oracle in shadow) - outside the allowlist; only coarse `conf_bucket` is logged.

**H. Router coverage / security / calibration (§10, m01041 SO 0.937)**
28. **SO `state` = trust boundary:** only the user request enters; MRTR `input_required` nested `systemPrompt`/`message` never enters (prompt injection). `secret-clean` BEFORE merging. If no capability gating, -32602. Unknown `resultType` + generic Exception -> passthrough; NEVER 503.
29. **Calibration (offline, no live SO):** pure-Go isotonic PAV + ECE target 0.12->0.03; `quantile(1-strong_model_pct)` instead of fixed 0.8; parse error -> strong pin (absence != low); if the R_zero baseline cannot be renewed, revert to 0.8; `conf-rule` tied to the cache key. No Rego/CEL -> plain `gate(layer0) bool`.
30. **Honest coverage ratio:** `(pre-model AND decision-authoritative AND relay-successful AND opt-in AND calibration-passed) / all requests` - intersection, NOT union. A client x authority matrix is kept; real local coverage is only OpenCode V2; others grow with opt-in adoption.

> Status: survival simulation (m00559, 8 ADHD agents) complete -> §8 + G items added (SO `complete` p=0.627). 17 router branches + 5 ADHD frames + 2 deepen + router audit complete. System One architecture vote round 2: **go_routerd_plus_ts_shim 0.939** (prob 0.954, >=0.8 = valid verdict). Round 1 weak choice (pure standalone proxy 0.731) was overridden.
> MCP-global research (m00251): 5 frames + 4 deepen + web evidence. System One: **koru_ve_mcp_extension_ekle 0.951** (prob 0.963); user claim **partly correct** (score 1.05, p=0.882). See §1b below.
> **GLOBAL PIVOT (m00377, SO 0.961/0.984):** no 9router/opencode-specific port. Single static Go binary = protocol-level global MCP relay; 9router/opencode is only an example deployment. **§0 and §1d below are current - the previous §0 text is historical.**
> Repo: `/Users/keremurhan/walsy-system1` - greenfield, 0 files. Secrets are never written to the plan/log.
> Scope (m00181): **System One router only.** lrecon = analogy, PARK (see §7 below).

## 0. Final architecture - GLOBAL MCP relay (System One approved, m00377)

Historical note: this used to say "OpenCode -> TS shim -> Go routerd -> 9router". The user (m00377) does not want a 9router/opencode-specific port; System One approved pivot **global_relay_arastir 0.969**, architecture vote **global_relay_core 0.961**, revised **0.984**.

```
any MCP client       ─┐
                      ├─→ [single static Go binary: JSON-RPC 2.0 relay core] ─→ MCP server
any MCP server       ─┘         ↑ DECISION (optional, opt-in)
```

- **Single static Go binary** - `CGO_ENABLED=0`, no libc, no external deps. `go install` / Homebrew / single-file download = same behavior. No config file: `os.Getenv("SYSTEMONE_ROUTE_URL")`, `os.Getenv("NINE_ROUTER_LOG")`.
- **SINGLE relay core, staged transports:** **v1 only (1) stdio child (`type:local`)**. On day 3: (2) Streamable HTTP (`type:remote`; no SSE) + (3) unix socket/FIFO file mode. No client-side special code - only the address in config changes (command/URL/file path). 9router/opencode is **an example deployment**, not a code path.
- **Protocol fidelity:** `id`, `_meta.progressToken`, `protocolVersion` preserved verbatim; on HTTP the `MCP-Protocol-Version` header is mandatory; `sampling`/`roots`/`elicitation` server->client reverse requests pass through transparently; **unknown capability -> pass byte-for-byte uninterpreted; only security/scope decisions are deterministically rejected (no silent swallowing means: a security decision never passes silently)**. Rationale: SEP-2577 roots/sampling/logging deprecated (confirmed by go-sdk README); 3am on-call: telling an unknown capability "desteklemiyorum" [EN: "I do not support it"] breaks the client.
- **Coverage mechanism:** `capabilities.experimental['com.kerem/systemone-route'] = {enabled:false, mode:"relay"}` is advertised at initialize (`schema.ts:310-314,386-390`, `additionalProperties:true`; no top-level `x-`). SDKs are default-off: routing is active on clients that opt in, **transparent relay** on those that do not. Output = **not a guarantee, but coverage ratio + rejection count**.
- **No `before_model` in the MCP spec** - the model call is outside MCP. "Her sistemde model-öncesi garanti" [EN: "pre-model guarantee on every system"] is impossible (System One p=0.718). Decisions only within MCP traffic; the real LLM guarantee is at the mandatory `:20128` pass-through (optional observation: `proxy.log` + `route_counts`).
- **Route order:** Layer 0 deterministic (normalized hash + static short-circuit, ~us) -> Layer 1 System One (only AGENTS.md label-shaped decisions, conf>=0.8) -> Layer 2 transparent passthrough (default).
- **Fail-safe:** `conf<0.8` / timeout / HTTP!=200 -> transparent passthrough. Fail-closed (503) is not the default - if the relay dies, the client is left without MCP; shadow mode first (call + log the decision, passthrough not broken).
- **No self-spawn** (m00376 SO 0.880), **no network gate** (0.008), **no LD_PRELOAD/eBPF** (breaks on signed binaries / locked to root+Linux = not "her sistem" [EN: "every system"]).
- An unnecessary System One call is a tax (~0.5-1.7s + 350 tok). Not asked on every request.

## 1. ADHD Phase 2 - 5 frames x 6 ideas + 2 deepen

Fragile assumptions: "Go = MCP server", "modelden önce = client hook" [EN: "before the model = client hook"], "plugin TS olmak zorunda" [EN: "the plugin must be TS"], "ayrı process" [EN: "separate process"], "request-response". None is required; the decision layer is in HTTP, the trigger in the plugin.

**Cluster A - move the interception point**
- HTTP middleware hard gate (model cannot skip) `[N8 V9 F10]` *
- Go binary = 9router itself `[N9 V6 F10]` - *trap: patch is fragile*
- provider request queue = "önce" [EN: "before"] `[N9 V6 F9]`
- fail-closed 503 `[N8 V7 F9]` - *trap: SPOF*
- single long stream, pipeline decision `[N7 V5 F7]`
- no TS plugin at all `[N6 V3 F8]` - *trap: invocation required*

**Cluster B - eliminate decision cost**
- singleflight: same state 99 copies -> 1 call, fan-out `[N8 V9 F9]` *
- deterministic short-circuit (/models, static, retry) `[N7 V9 F10]` *
- decision table in a single cache-line (8-byte key, atomic compare) `[N8 V8 F8]`
- branch predictor: pattern from last N decisions, no gate on repeats `[N8 V7 F9]`
- single thread + ring buffer (no spawn/GC) `[N7 V7 F7]`
- fixed 350-token prefix -> clef cache ~100% `[N9 V6 F8]` - *trap: prefix is not guaranteed*
- speculative execution `[N9 V5 F7]` - *trap: model already started*
- CPU pin / fork-server `[N6-7 V5-6 F6-7]`
- byte glitch (trim 1 character, Jaccard 0.999) `[N8 V4 F6]` - *trap: wrong route*

**Cluster C - turn guarantee into observability (3am)**
- shadow router (log-only, does not route) `[N8 V9 F5]`
- embed config in the binary (no disk file) `[N7 V9 F7]`
- self-shutdown -> traffic goes straight to main `[N8 V8 F6]`
- fully stateless (no health.json/breaker) `[N7 V8 F6]`
- silence contract `[N9 V8 F4]`
- bypass header `[N7 V8 F8]`

**Cluster D - against poisoning**
- state in untrusted field, instructions in separate field (injection broken) `[N7 V9 F9]`
- `Secret` type + redacted `String()`; key via stdin/exec, no env/arguments `[N6 V9 F8]`
- schema in binary, config cannot override; version stamp on every response `[N7 V8 F8]`
- reverse flow / namespace slip / let clef produce the decision itself - **all traps** (guarantee illusion)

**Convergence:** Obvious but ordinary: single cache-line table, shadow router. **Non-obvious-but-viable \*: Go decision + 10-line TS trigger + Cluster B/C synthesis** - guarantee at the architecture layer, cost zero on repeats, SPOF managed with shadow mode.

**Deepen 1 - HTTP middleware hard gate (fail-closed):**
9router :20128 sees all traffic; the decision moves from the MCP/plugin layer to the HTTP layer. Request -> System One `/v1/systemone` (clef) -> if "devam" [EN: "continue"], upstream; if "red" [EN: "reject"] / no response, 503. To avoid double voting, the existing plugin vote is removed; the daemon is the single decision source. But since every request would add ~350 tok + latency, most requests must pass without a call. The daemon is a single point of failure -> shadow mode first. *Load-bearing risk:* SPOF (System One 0.816). *First step:* add middleware in shadow mode, log the decision, do not return 503 (confidence 0.985). *Sub-ideas:* health check + passthrough with the cached last decision; cache-first TTL map; bypass header; plugin only reads metrics; shadow log schema (label/confidence/latency/hash).

**Deepen 2 - cache-only hard gate (deterministic short-circuit + fixed prefix + singleflight):**
Request intercepted -> normalized hash -> L1 atomic decision (~us) -> known static/repeated request from cache, no model call -> genuine new label-shaped request -> single goroutine to System One via singleflight, 99 copies fan out -> conf<0.8/timeout -> main. *Load-bearing risk:* **state must be in the cache key -> hit ~0 on unique requests.** "Near-zero System One çağrısı" [EN: "near-zero System One calls"] only holds for retry/restart/fan-out repeats; dropping state silently routes wrong. Second break: no guarantee without being in-path, and no guarantee when fail-open. *First step:* count the `sha256(state)` uniqueness ratio of the last 1000 requests from the 9router access log; if >95%, skip writing L1 and go straight to singleflight (20-line script, decides without setting up a daemon). *Sub-ideas:* L1 only for retries; compile-time decision table (Go switch/enum); sidecar UDS + plugin socket experiment; key singleflight on the `(state, questions)` pair with 100ms coalescing, remove L1; one week of telemetry only ("hit olsaydı" [EN: "if there had been a hit"] fake accounting), no short-circuit.

**Provocation:** Are "Garantili model-öncesi" [EN: "guaranteed pre-model"] and "her istekte System One" [EN: "System One on every request"] the same thing? No - guarantee = *every request passes through the daemon*, System One call = *only when needed*. No cache/dispatch code is written until this distinction is clear.

## 1b. MCP-global proxy research (m00251, System One approved)

**User claim:** "MCP proxy olarak yazılırsa daha global ve herkese uygun olur, 9router/OpenCode'a özel yazılmaz." [EN: "If written as an MCP proxy it would be more global and suitable for everyone, not specific to 9router/OpenCode."] -> System One: **partly correct** (score 1.05/2, p=0.882). What is correct: MCP's transport-agnostic JSON-RPC 2.0 layer really is client-agnostic. What is wrong: this layer cannot provide a *pre-model guarantee*.

**Why it cannot (spec evidence):** MCP 2025-11-25 has no `before_model` phase. The model call is outside MCP, in the client's head. The server only offers tools; the model chooses whether to call them. `sampling`/`roots`/`elicitation` are server->client reverse requests - the proxy passes them but cannot mandate them. Unknown capabilities are passed byte-for-byte; only security/scope decisions are deterministically rejected (no silent swallowing means: a security decision never passes silently).

**Decision (SO 0.951):** Current §0 architecture is KEPT (Go routerd decision + TS shim trigger). A client-agnostic MCP JSON-RPC relay is added on top - **not a fully global proxy**, data plane global / trigger plane client-specific.

```
DATA plane    → Go MCP relay (v1 stdio; day 3 Streamable HTTP, spec-faithful) — client-agnostic
TRIGGER plane → existing 10-line TS shim (OpenCode) — client-specific, guarantee HERE
MEASUREMENT   → "coverage ratio": how many clients opened the extension, logged
```

**Spec-legal extension (verified via webfetch):**
- MCP extensions are the official mechanism: SEP-2133 (Final) + `https://modelcontextprotocol.io/extensions/overview`.
- ID format `{vendor-prefix}/{extension-name}`, reverse-domain required by recommendation -> `com.kerem/systemone-route` (assumes namespace ownership; change if not corporate).
- Placement: under `capabilities.experimental` - `schema.ts:310-314,386-390` -> `experimental?: {[key:string]:object}`, `additionalProperties:true`. **No top-level `x-` field**; a strict validator may reject it.
- SDKs implement extensions as **default OFF + explicit opt-in**; extension support is NOT REQUIRED for 100% protocol conformance -> clients without support do not reject initialize, they ignore it. "Herkes bağlanabilir" [EN: "everyone can connect"] is true; "herkes route edilir" [EN: "everyone is routed"] is false.
- `before_model` is still absent -> the extension only carries discovery/coverage, **zero enforcement**. The guarantee stays in the TS shim. Anything requiring a new capability negotiation field needs a SEP (`/community/sep-guidelines`).
- Relay rules: take `initialize` protocolVersion from the client (no hardcoding; on HTTP the `MCP-Protocol-Version` header is mandatory on subsequent requests); copy JSON-RPC `id` + `_meta.progressToken` verbatim; pass `sampling`/`roots`/`elicitation` through uninterpreted; **unknown capability -> pass byte-for-byte uninterpreted; only security/scope decisions are deterministically rejected (no silent swallowing means: a security decision never passes silently)**. Rationale: SEP-2577 roots/sampling/logging deprecated (confirmed by go-sdk README); 3am on-call: telling an unknown capability "desteklemiyorum" [EN: "I do not support it"] breaks the client.
- Sources: `/extensions/overview`, `/seps/2133-extensions`, `/specification/2025-11-25/basic/lifecycle`, `/community/sep-guidelines`, `schema.ts` (2025-11-25).

**Traps (rejected):** fully global proxy (SO 0.013 - technically impossible); network gate/DNS+307 (SO 0.008 - DoH/DoT leak, SPOF, MITM CA, breaks SSE); `x-systemone-route` top-level field (not in the spec); "spec-yasal = garanti çözüldü" [EN: "spec-legal = guarantee solved"] illusion.

**Relay self-check:** the single acceptance test proving that a client without an extension completes `initialize` + tool call without issues; assert that on a client with the extension the `com.kerem/systemone-route` marker appears in `_meta`/`capabilities.experimental`. Without this test, the "herkese uygun" [EN: "suitable for everyone"] claim is rhetoric.

## 1c. Self-spawn proxy research (m00326, System One approved)

**User question:** "MCP çalışırken kendi proxy'sini açsa, paketlenen lrecon gibi Go binary'sini çalıştırsa, her isteği otomatik algılasa — mümkün mü?" [EN: "If MCP opened its own proxy while running, launched its packaged Go binary like lrecon, and automatically detected every request - would that be possible?"] (lrecon is only an analogy, PARK, out of scope.)

**ADHD:** 5 frames x 6 ideas (10-year-old, game design, ant colony, $0/1h, attacker) + 3 deepen.

**Mechanical answer: POSSIBLE, BUT WRONG SCOPE.** Writing the binary with `go:embed` to a 0700 `mkdtemp` directory on the first MCP call and `exec`ing it works technically. Parent PID liveness + lineage chain + inode/SHA-256 pinning + HMAC channel + single-use nonce can be added. **But** this proxy only sees traffic passing through MCP stdio; model HTTP calls go over `:20128`, **outside** MCP -> "her isteği otomatik algılama" [EN: "automatic detection of every request"] has **NO GUARANTEE**, security theater.

**System One decision:** `koru_self_spawn_yok` **0.880** (prob 0.920) - do **NOT ADD** self-spawn, current §0 architecture is kept. The claim is `kısmen` correct (argmax p=0.767, weak conf 0.651 -> settled with YAGNI + spec evidence).

| Option | SO probability | Result |
|---|---|---|
| `koru_self_spawn_yok` | 0.920 | **ACCEPT** - current daemon + relay is enough |
| `sadece_tee_gozlem` | 0.064 | rejected - measures MCP traffic, not LLM traffic, extra code |
| `ekle_self_spawn` | 0.017 | rejected - same coverage at higher cost + SPOF + attack surface |

**Real source of "her LLM isteği" [EN: "every LLM request"]:** the mandatory `:20128` 9router pass-through (headroom `proxy.log` + `route_counts`), already in the always-running launchd daemon. No need to open a new process.

**Traps (rejected):**
- Thinking self-spawn "her isteği yakalar" [EN: "catches every request"] = coverage illusion.
- Lineage-tree cryptography = new attack surface that carries no load.
- Putting inode/hash pinning **after** `exec` = too-late protection; predictable extract directory = PATH/host hijack.
- Presenting the coverage ratio as a guarantee = hides silent requests; rejection count must be reported explicitly.

**No task added:** `mcp_relay.go` / TS shim are already in §1b. **No** new file, process, or dependency for self-spawn. If desired, one observation only: compare the stdio JSON-RPC line counter with `proxy.log route_counts` and report the coverage gap numerically (measurement, not code).

## 1d. Global MCP proxy decision (m00377, System One approved)

**User:** "9router+opencode portu yapmacaksın — bu global bir MCP proxy olacak, her sistemde çalışan, bağımlılığı pek fazla olmayan." [EN: "You will not make a 9router+opencode port - this will be a global MCP proxy, running on every system, with few dependencies."] -> The previous 9router/opencode-specific daemon+shim description is **invalid**, §0 updated.

**System One rounds:**
| Round | Decision | Result |
|---|---|---|
| Pivot (m00382) | `global_relay_arastir` **0.969** (p 0.980) | Accept - no custom port, research global relay |
| Pivot (m00382) | "her sistemde model-öncesi garanti" [EN: "pre-model guarantee on every system"] `imkansız` p=0.718 (weak conf 0.516) | Direction clear: coverage ratio instead of guarantee |
| Architecture (m00413) | `global_relay_core` **0.961** (p 0.974) | Single binary + 3 transports + opt-in |
| Revised (m00413) | `evet_revize` **0.984** (p 0.992) | plan.md §0/§1d updated |

**ADHD:** 5 frames x 6 (remove-assumption, $0/1h, logistics, 3am, biology) + 3 deepen (transport/packaging/coverage).

**Clusters (N.35+V.40+F.25, excluding traps):**
- **Eliminate the transport layer:** FIFO/unix socket file mode `[N7 V8 F9]`, socket activation (launchd/systemd fd handoff) `[N8 V7 F8]`. Trap: LD_PRELOAD/DYLD `[N9 V5 F8]` breaks on signed binaries; eBPF `[N9 V3 F7]` locked to root+Linux -> "her sistem" [EN: "every system"] is a lie.
- **Single binary, zero config:** multiple identities via `argv[0]` `[N8 V9 F9]`, `CGO_ENABLED=0` static binary `[N6 V9 F9]`. Trap: c-shared dylib brings back the LD_PRELOAD dependency.
- **Protocol legitimacy / coverage:** `capabilities.experimental` opt-in + transparent relay fallback `[N7 V9 F10]` *, roots for cwd->project root `[N8 V7 F8]`, coverage ratio + rejection count `[N6 V9 F9]`.
- **Self-repair (3am):** MCP `ping` heartbeat + capability re-negotiation `[N8 V8 F8]`, carry state via `progressToken` (no DB) `[N7 V8 F7]`, encode health in `resources/list` URI `[N6 V8 F6]`. Trap: mistaking the host LLM for a router via `sampling/createMessage` - reverse requests carry refusability, not a guarantee.
- **Biology (wild):** anergy (staying silent then waking), quorum threshold, molecular mimicry, red-queen, sparse coding, plasmid capability transfer - all smell of **manipulation**, rejected.

**Converge (\* = non-obvious but viable):**
1. \* **Global relay core** - single JSON-RPC core + 3 transport adapters, client-agnostic, zero dependencies. SO 0.961.
2. \* **`capabilities.experimental` opt-in** - reduces the "her sistemde çalışır" [EN: "works on every system"] claim to the level of *"her sistemde en azından şeffaf geçer, destekleyenlerde yönlendirme yapar" [EN: "at least passes through transparently on every system, and routes where supported"]*. Coverage ratio, not a guarantee.
3. **Single static binary + env config** - no file/config layer; `go install`/brew/single download. Risk: the daemon does not restart when env changes (restart dependency), allowlist needed for HTTP remote.

**Traps (separate, with rejection rationale):**
- Self-spawn backdoor - rejected with m00376 SO 0.880 (coverage illusion).
- "Model-öncesi garanti" [EN: "pre-model guarantee"] - no `before_model` in the MCP spec, impossible.
- Network gate / DNS/307 - SO 0.008; DoH leak, SPOF, MITM CA.
- Presenting the coverage ratio as a guarantee - hides silent requests; rejection count must be reported explicitly.
- eBPF/LD_PRELOAD - breaks the "her sistem" [EN: "every system"] claim.

**Focus - summary of 3 deepen:**
- **Transport:** `ReadFrame/WriteFrame` single contract; **v1 stdio only**; on day 3 unix socket shares the same implementation + Streamable HTTP (MCP-Protocol-Version in one place; no SSE). Risk: cannot verify the decision was applied (model outside MCP) + SDK default-off silently disables it.
- **Packaging:** `CGO_ENABLED=0` + `os.Getenv` single init; `mcp_relay_test.go` asserts env set/unset. Risk: self-spawn trap, env restart dependency, HTTP remote allowlist.
- **Coverage:** `{enabled:false, mode:"relay"}` sample payload at initialize; without client opt-in the field is ignored. Risk: coverage ratio may be ~0 on day one, rejection count invisible without measurement.

**Provocation:** If a guarantee is impossible, should the product be named "router" or "gözlemlenebilir kapsam ajanı" [EN: "observable coverage agent"]? Which promise is the user buying: routing or visibility?

## 1e. Provider registry / ranking / port fallback (m00445, 16 agents)

**User (m00445, Turkish summary):** MCP proxy runs on localhost, a small System One call site like 9router; add and **rank** providers; dedicated localhost endpoint for System One models (v1m/Evren); occupied port -> automatic free-port fallback. 9 ready providers: OpenRouter(Connected), Ollama Cloud(none), OpenCode Free(Ready), v1m System One(none), Evren(System One)(Connected), Nace Drex, Ollama Local, Cloudflare, OpenCode Zen + individual add slot.

**ADHD:** 13 diverge x 6 ideas + 3 deepen = 16 agents (research-only). Score set: portability/registration safety N.33+V.38+F.30, scoring/SO mechanisms N.10+V.4+F.3.

**System One rounds (transparent record):**
| Question | Round 1 | Round 2 (tighter state + security checks) | Decision |
|---|---|---|---|
| Package acceptance | `kismi_kabul` 0.274 (weak) | `tumu_kabul` 0.659, p=0.773 (**weak**, <0.8) | Ranking+registry accepted; port separate |
| Ranking | `sticky_pin_rotation` 0.937, p=0.958 | not asked again | **CLEAR - accepted** |
| Port | `bind0_discovery` 0.740 (weak) | `mevcut_tasima` 0.299, p=0.533 (**weak + contradictory**) | **NO new port code** |

> Weak-verdict rule: not irreversible -> retried once with tighter state; still <0.8 and contradictory, so the port was decided with **evidence + YAGNI**, model vote not shown. Ranking (0.937) is the only strong verdict.

**Accepted mechanisms:**
1. **Ranking = sticky pin + deterministic rotation - no score, no System One.** Single evidence bit: last successful provider ID as one string in process RAM (disk I/O is YAGNI). The request uses the pin; on the first failure the pin is dropped and the next provider is chosen by `hash(sorted(reachable IDs))` order. No clock/latency/health-probe. After restart there is no pin -> config order; the first success writes the RAM pin. Pheromone / tick-size / margin-collateral / static-priority / "her çağrıda SO" [EN: SO on every call] = YAGNI - they merely reproduce the weak 0.51 score in another form (SO 0.937 confirmed this).
2. **Registry = compile-time `.rodata` 9-element slice + one-line `providers.txt` (#10 slot).** Each line has 3 fields: `name, base URL, key env name` - **key is never written to the file**, it comes from env (single source of credential). Order = file line order, or the baked 9 order if the file is absent; reordering = moving a line, no code change. No new MCP mutation protocol.
3. **Dedicated endpoint for System One = `SYSTEMONE_ROUTE_URL` env.** If set, v1m/Evren System One models are routed there; otherwise the normal provider. No separate mutable layer/negotiation (the constraint already excludes capability negotiation).

**Security (trust boundary - cannot be skipped):**
- `providers.txt`: `0600` + fixed path + **symlink rejection**. If a local attacker writes a line, they redirect traffic to their own URL / change the key env name to prepare credential exfiltration.
- Pin: single string in process RAM (disk I/O is YAGNI; after restart config order, first success writes the RAM pin).
- Endpoint validation: **loopback Host check only** (non-loopback Host -> passthrough; if needed for DNS rebind protection). Never log secrets.

**Port fallback decision (evidence + YAGNI) - deferred to day 3 (v1 is stdio only):** No `Port++` retry loop, no `SO_REUSEPORT`, no separate discovery file. The user request "dolu port → otomatik boş port" [EN: occupied port -> automatic free port] is met as follows:
- **stdio / unix socket:** no TCP port at all - collision is conceptually impossible.
- **HTTP mode (day 3):** `transport_http.go` calls `net.Listen` with `127.0.0.1:0` instead of a fixed port; **the OS picks a free ephemeral port at that moment** - this is "otomatik boş port" [EN: automatic free port] itself, with no scanning/retry code. The chosen address is written as the `listen_addr` field into the already-required `metric.go` stderr JSON line (not a new trust boundary - the same allowlist channel); the client/supervisor connects from that line. The `getsockname()` result is one line, secret-free. If a fixed port is preferred, it can be overridden with the `MCP_PROXY_PORT` env (then a collision is the user's choice, no retry).

> This decision was made with evidence + YAGNI because the SO port votes were **weak/contradictory** (round 1 `bind0_discovery` 0.740, round 2 `mevcut_tasima` 0.299/p=0.533). The existing stderr channel was used instead of a separate discovery file, minimizing the contradiction between round 2's argmax (`mevcut_tasima`) and the new code.

**Load-bearing risk:** the pin can get stuck on a provider that does not error but is slow (degraded) - rotation is not triggered; the definition of "başarı" [EN: success] (is 2xx enough, or a latency threshold?) must be clarified. Day-1 coverage ratio may stay ~0 (§1d).

**Acceptance test (`provider_test.go`):** (a) baked 9-order when `providers.txt` is absent, (b) adding one line makes #10, (c) no pin after restart -> config order, (d) key env name not visible in the file, (e) symlink rejected, (f) rotation on first failure is deterministic and the same order for the same state, (g) HTTP `listen_addr` loopback + port>0 and no secret in the stderr metric line.

## 2. Cache reality (verified - read-only)

- **9router `semanticCacheEntries`: DEAD SCHEMA.** Table exists (key/model/responseBody/hitCount/tokensSaved + 2 indexes), `count(*)=0`, **0 references** in `~/.9router/runtime/**/*.js`. Layer 0 cannot use it; no patch is written.
- **Headroom `semantic_cache.py`: code active, 0 hits.** PID 50029 = `python -m headroom.cli proxy --port 8180 --code-aware`; `proxy/server.py:1196-1203` has a cache instance, defaults `cache_enabled=True`/TTL 3600/LRU 1000; `/health` healthy. But `/metrics` -> `headroom_requests_cached_total 0`, PERF log -> `cache_read=0 cache_write=0 cache_hit_pct=0`. Cause: `not stream` condition in handlers (`handlers/anthropic.py:1445`, `handlers/openai.py:3930`) + per-process HMAC partition. The in-process `OrderedDict` -> cannot be read from the router over HTTP.
- **`cache_hit`/`cache_miss` in the logs are a different layer:** `headroom/transforms/content_router.py:1586-1598` (local transform cache, ~54us, 79 hits/1 miss). `cross_turn_dedup` is there too. Unrelated to `semantic_cache.py`. `headroom.content_router` is not touched.
- **Conclusion (item 13 pivot, §9):** the router **builds its own decision cache** - it does not attach to either existing cache (dead schema / 0 hits). The old in-process `cache_store` produced self-validating metrics; the new design fixes this with **counterfactual replay + per-phase counters** (item 14). What actually determines the hit rate: **removing `state` from the key** - that was the root cause of 0 hits (§2: with `state` in the key, hits on unique requests are ~0). Branch 5/7 normalization/singleflight patches can be written to headroom `semantic_cache.py` - separate, optional work; the router decision layer is unaffected.
- **What a cache hit brings:** it **skips the decision call** to SO (saving 1500ms + 1 round trip) and speeds up traffic going to the same provider via the sticky pin. Gain = decision latency + SO load; **caching of the answer is not claimed** (only the typed label; prose is regenerated on every hit). The "hit ~100%" claim is still not used - the real hit candidate is first measured in 3 days of shadow mode.

## 3. Branch findings - build / skip (audit corrected)

**Build (inside Go `routerd`, single binary):**
- Tool selection + parameter validation (branch 8) - allowlist/scope first, deterministic; JSON Schema ~20 lines; many candidates + unresolved -> System One `choice`. **System One never decides permission/scope.** Tool output/raw user text does not enter `state`. `conf>=0.8 && argmax`, 1 retry (6s), then deny/ask. Fallback `openrouter .../v1/systemone`, model `typesafe/jev-1.14`. Unknown capability -> pass through byte-for-byte without interpretation; only security/scope decisions are deterministically rejected (meaning no silent swallowing: a security decision never passes silently). Rationale: SEP-2577 roots/sampling/logging deprecated (confirmed by go-sdk README); 3am on-call: "desteklemiyorum" [EN: I do not support it] breaks the client.
- Cache policy: **NONE** - no decision cache added (see checklist item 13); it is not added before real uniqueness is measured on day 3.
- Observability (branch 15): 1 JSON line to stderr, allowlist `tool/route/miss_reason/decision/model + ts/dur_ms + key_hash(12 hex) + listen_addr + seq/pid + reason_code/covered/abstained/passthrough`. **Raw** `conf`/`tokens`/`cost` unverified (a claim of its own process, no oracle in shadow) - outside the allowlist; only the coarse `conf_bucket` is logged. **Forbidden:** args, prompt, context, header, env, raw key. `jq -s` report. `miss_reason` and `gate_called` ratio are reported.
- Health/fallback (branch 11): `classify(status,exc)` transient `408/429/500/502/503/504`+timeout, permanent `400/401/403/404/422` - KEPT for the passthrough reason-code. Single bool `so_degraded`: one timeout/HTTP!=200 -> passthrough, no retry. Remove breaker / state machine / `health.json` (file I/O is YAGNI). If the backup is on the same :20128, failover is meaningless (correlated failure).

**Skip (NOT in the first version):**
- New HITL code (branch 9) - `walsy-reflex v0.4.0` already does 80%; only 4 hard categories are added to `policy.yaml`. HARD GATE applies to irreversible tasks.
- Session memory (branch 12), rate limit/quota (branch 10), common multi-provider `generate()` interface (branch 2) - if existing infrastructure exists, go through it; no new files. No evidence for "Jev"; do not assume it.
- Prefilter (branch 6) System One gate - only 1000-4000 chars, no errors, metadata-only; `conf<0.8` -> trim. If needed, ~20 lines in Go, no separate `.mjs`.
- Layer 0 on top of 9router `semanticCacheEntries` (branch 5 - dead schema), patching 9router.

**Endpoint (settled):** `POST https://evren-llmapi.ssyz.org.tr/v1/systemone`, header `x-api-key` from env, `model:"clef"`. The `localhost:20128/api/v1/systemone` + `model:"systemone"` variant (branch 13) is not used - 9router patches are removed on update.

## 4. System One common specification

```
{ "model": "clef", "state": "...", "questions": { "<q>": { "type": "choice|score", ... } } }
```

- `questions` is an object (not an array). Only `choice` and `score`.
- `score` -> 0..1 float + `legend` + index-keyed `probabilities`; label from `legend`/argmax.
- `confidence < 0.8` = invalid verdict: reversible -> retry once with a tighter state, irreversible -> stop + human approval.
- Timeout / HTTP!=200 / parse error -> fail-safe default (defined in each branch).
- `state` contains everything (the model has no memory), but never secrets/PII/long context; untrusted content is in a separate field.

## 5. File map (Go - final)

```
walsy-system1/
  go.mod                      # module global-mcp-relay, stdlib only, CGO_ENABLED=0
  main.go                     # single binary: select transport (stdio|http|unix), env config
  mcp_relay.go                # JSON-RPC 2.0 core: copy id/protocolVersion/progressToken (§0/§1d)
  transport_stdio.go          # type:local child - written FIRST
  transport_unix.go           # unix socket/FIFO - day 3
  transport_http.go           # Streamable HTTP, MCP-Protocol-Version - day 3 (no SSE)
  route.go                    # Layer 0/1/2 + fail-safe (opt-in)
  gate.go                     # System One choice/score, timeout 1500ms, no retry
  guard.go                    # classify + single bool so_degraded (passthrough)
  metric.go                   # stderr allowlist JSON + coverage ratio + rejection count
  provider.go                 # compile-time 9 slice + providers.txt(#10) + sticky pin/rotation (§1e)
  route_test.go               # assert-based self-check
  mcp_relay_test.go           # transport-agnostic frame round-trip + extension marker (§1b)
  provider_test.go            # 9 baked / +1 line / no pin after restart -> config order / symlink rejection / rotation (§1e)
```

**Removed (global pivot, m00377):** `plugin.ts`, `package.json`, `config.json`, `.env`, `com.kerem.routerd.plist` - no 9router/opencode-specific distribution layer. Config = `os.Getenv("SYSTEMONE_ROUTE_URL")`, `os.Getenv("NINE_ROUTER_LOG")`. Distribution = `go install` / brew / single-file download; service management is the client's own mechanism (launchd/systemd optional). Also gone: `router.py`, `guard.py`, `memory.py`, `proxy.py/.mjs`, `prefilter.mjs`, `approval.mjs`, Rust `reflexd` integration, `semanticCacheEntries` layer.

## 6. Implementation order (measure before writing code)

1. **Shadow mode (3 days):** connect the single binary as an MCP client's `type:local` command, for 3 days only log decisions + transparent pass-through. Do not enable 503/fail-closed. stderr append-only JSON line `{seq,pid,ts,decision,reason_code,covered,abstained,passthrough,latency_ms}` - `seq` gap = `pid` to distinguish drop/restart (no separate receipt file). See distribution + latency + `gate_called` ratio.
2. **Uniqueness measurement - deferred to day 3:** `sha256(state)` ratio of the last 1000 requests. No cache is added before this measurement (item 13).
3. **Transport order:** **v1 only `transport_stdio.go`** + round-trip test. On day 3, `transport_unix.go` + `transport_http.go` (Streamable HTTP, MCP-Protocol-Version in one place); no SSE. `argv[0]` multi-identity later (YAGNI). In HTTP mode no fixed port: `127.0.0.1:0` + OS ephemeral selection, `listen_addr` written to the stderr metric line (§1e). No Port++ retry / SO_REUSEPORT / separate discovery file (item 12, day 3).
4. **MCP relay test:** client without extension completes `initialize`+tool call; client with extension sees the `com.kerem/systemone-route` marker (§1b); `id`/`progressToken`/`protocolVersion` are preserved.
5. **Provider test:** `provider_test.go` - 9 baked / one line -> #10 / no pin after restart -> config order / symlink rejection / deterministic rotation (§1e).
6. **Go test:** `CGO_ENABLED=0 go build ./... && go test ./...` (route/gate/guard/mcp_relay/provider self-checks; env set/unset asserts). No guard state test (state machine removed).

Cut: 9router/opencode-specific TS shim + daemon plist + config.json/.env, self-spawn, network gate, LD_PRELOAD/eBPF, new MCP server, provider/cache layer fork, new HITL/memory/rate-limit code, embedding/vector DB, all `.py`/Rust paths. Risk: no pre-model guarantee (no `before_model` in the spec) - the product must be positioned as a "görünür kapsam ajanı" [EN: visible-scope agent]; SDK default-off -> day-1 coverage ratio may be ~0, misleading if the rejection count is not reported; env config creates a restart dependency.

## 7. lrecon - PARK (m00181)

Result of 16 ADHD branches + 3 weak System One rounds (all <0.8): **no new Go tool, no separate binary, no extension for now.** Existing `/Users/keremurhan/walsyrea/env/lrecon/` (635 `.go`, ~138k LOC, 235 `--op` + 80 `--ext-op`) is used as-is. With m00196 only an *analogy* ("daima çalışan" [EN: always running]), out of scope. Notes: docs pin stale (`op-table.md:53` `238a7be6…` vs shipped `60581ed8…`), `native.go:1564` missing shell-quote, `op_extract_firmware.go:50` `checkMember` unverified - all separate work, not in this plan.

## 8. Survival matrix - "kurulu ama bozuk" [EN: installed but broken] (m00559, SO `complete` p=0.627)

Single invariant (item 6): regardless of the SO layer's state, client<->server traffic passes through byte-for-byte. **No 503/fail-closed, first request is not delayed.** The rows below are examples of item 6 - no separate code.

| State | Result | Additional behavior |
|---|---|---|
| cold start / no requests yet | no SO call, as if proxy were not installed | de-energized start |
| warm-up clef 1500ms timeout | passthrough | late response discarded, no retry |
| SO timeout / provider 500 | passthrough | fixed `so_unreachable`; no URL/header/key in logs |
| HTTP200 + partial body (brown-out) | "karar yok" [EN: no decision] -> passthrough | not counted as success |
| conf 0.799 / 0.801 | passthrough / pin respectively | threshold deterministic |
| sticky pin death | pin -> passthrough | separate reason-code, no self-restart |
| key env empty | passthrough | `degraded` != `opt-out`, separate log |
| hot reset | passthrough | epoch increments, old verdict does not leak; per-request relay, no global lock |
| busy request | passthrough | a single request does not lock the whole flow |
| late response at 1499ms | discard | hard budget wins |

Evidence layer in item 27: **single schema** - v1 live = single stderr reason-code line; 3-day shadow = stderr append-only JSON line `{seq,pid,ts,decision,reason_code,covered,abstained,passthrough,latency_ms}` (no separate receipt FILE; `seq` gap = `pid` to distinguish drop/restart) + negative control. Addition to item 19: passthrough ratio + typed reason-code are first-class signals, baseline-deviation alarm. **Raw** `conf`/`tokens`/`cost` unverified (a claim of its own process, no oracle in shadow) - outside the metric allowlist; only the coarse `conf_bucket` is logged. Addition to item 9: at startup only a loopback Host check (remove canonical fingerprint / per-process token). Shutdown: leaves no pending request/state residue, no retry storm.

**Deferred to day 3 - no code now (YAGNI):** hysteresis + distribution against 0.81 poisoning, liveness heartbeat (frozen vs busy, diagnostics-only), carrying the receipt across epochs. Added then if needed.

**Honest boundary:** the "bağlanmama" [EN: not connecting] event may be unobservable to the proxy. Promise = "bağlanıldığında şeffafım" [EN: I am transparent when connected] + coverage ratio + rejection count.
## 9. Decision cache - main goal (m00752, SO pivot 0.980, §9)

Two separate mechanisms; both fail-safe passthrough (no 503).

**A. Relay decision cache (item 13):** store SO's typed decision (label + conf bucket + rubric version) under a canonical state key. Hit = pure local read -> the 1500ms SO call is skipped. Only the typed label is stored; prose is regenerated on every hit (stale summary impossible). Key = `hash(canonicalize(state) + rubricVersion + model + conf rule + registryVersion)`. Rubric alone is not enough - the SO label depends on state, and rubric-only replay gives the wrong label for a different state; raw state alone also gives 0 hits (the current headroom error). When rubric/deploy changes, old cells deterministically miss. A negative verdict (`no`/`other`/`conf<0.8`) is written as an `abstain` code and never served as a label. Admission: `conf>=0.8` AND probability margin >= threshold (clef is overconfident at 0.97-1.0 -> a single wrong label must not spread to all clients).

**B. Provider prefix stabilization - the real source of LLM cache hits:** the accepted summary+score is appended at a fixed position at the END of the same request as an **immutable block** (tidebreak 0019: compaction is not a separate request, it is the end of the same request). `system`/`tools`/`messages` byte-for-byte identical, tool array in fixed order (dynamic shrinking kills hits) -> the provider's own prefix cache kicks in (Anthropic 5min TTL, read 0.1x / write 1.25x; OpenAI min 1024 tokens). Layers: L1 full state hash (0 network) -> L2 question+criteria schema hash (reuse summary/score for the same rubric with a different payload) -> L3 embedding similarity **OFF**.

**Byte-stability mechanism (m00894/SO 0.888):** how to pass the determinism test - no `map[string]any` marshal, struct with fixed field order + a single canonical encoder (`SetEscapeHTML(false)`, sort **AFTER** canonicalize, `FormatFloat('g',-1,64)` + `-0` fold + NaN/Inf passthrough). No NFC/NFD in stdlib -> L1 raw + L2 `unicode.SimpleFold` aliases. Tool array snapshot + freeze at session start (`defer_loading:true`/`tool_addition`, all deferred -> 400); append-only generation order because the Go SDK recomputes `sortedKeys` on every add/remove. Breakpoints aligned to 512/1024/2048/4096 tokens in the tools->system->messages hierarchy; sglang page-alignment (1 token of leakage dirties a page) + a fixed guard-token sequence at the end of system (vLLM lookahead hash). A SHA-256 canary of the prefix's last bytes catches drift before sending.

**Load-bearing risk - byte drift:** the summary changing on every request / tool order slipping / the block moving from its fixed position -> every call is 1.25x WRITE, 0.1x READ never happens; instead of a hit increase there is a net cost loss. Even if the local hash stays constant, the provider/SDK may apply its own canonicalization and re-serialize bytes (UNVERIFIED) - the canary catches this only in the bytes actually sent. Determinism test BEFORE implementation: SHA-256 the serialized byte sequence twice for the same state+questions+criteria+tool-set -> they must be equal; if they diverge, report which breakpoint broke.

**Evidence (items 14-16) - TWO separate counters (m00894/SO 0.888):** router decision-cache hits (`reuse_precision` / `wrong_reuse`) and provider KV hits (`match_prefix` prefix_len/prompt_len, `BlockStored(block_hashes,parent_block_hash)`) are **never merged into one number** - merging would make the claim unfalsifiable. High provider + low decision = distinguishing feature missing; the reverse = prefill expensive. The Belady ceiling is computed offline from the replay block-hash sequence; the oracle separates the miss label `missed_reuse` (avoidable) from `model_changed`/`tools_changed`/geo/TTL (unavoidable). Threshold sweep 0.70-0.75 -> hit up/accuracy down curve; 384 calibration / 1152 test + revision hash (threshold does not leak into the test). Counting rules: `respjson.Field.Valid` distinguishes field absence from 0, SSE delta usage is **CUMULATIVE** (not summed), `stream`/`stream_options` are NOT cache-affecting (no need to split stream hits), truncated SSE usage = `unknown`. Falsification: if hits increase but `wrong_reuse` increases or accuracy drops, the claim is rejected. 3-day shadow: key + hit/miss + margin side by side, traffic unaffected. **Raw** `conf`/`tokens`/`cost` outside the allowlist; only the coarse `conf_bucket` is logged.

**SKIP (m00894/SO 0.151):** per-org namespace partition + idle warming/prefetch + sibling cache process (self-spawn ban) - not now, day 3 if needed.

## 10. Router scope / security / calibration (m01041, SO 0.937)

16 branches + 3 deepen; key finding: client hooks are either a spawned process (FORBIDDEN) or lack decision authority -> scope expansion stays in the relay/MCP layer.

**Trigger surfaces (Cluster A):** the only local surface that is both pre-model AND decision-authoritative is OpenCode V2 - `ctx.session.hook('experimental.model.resolve', ...)` (event.model mutate, same-provider, kind=primary|compaction|title|generate). Gemini CLI `BeforeModel` (llm_request override / synthetic llm_response / deny) is real but requires runtime embedding [UNVERIFIED]. Claude Code `UserPromptSubmit` is pre-model BUT context-only (prompt unchanged, no routing); `PreToolUse` is post-tool; `PermissionRequest` does not honor exit2. Cursor `beforeSubmitPrompt` is continue/user_message-only. Cline `beforeModel` is SDK/CLI/Kanban-only (no VS Code), file hook `cancel:true` blocks but does not mutate params; Roo has no hook API (#10834). Windsurf/Devin: 8 lifecycle hooks, no inference event. VS Code Copilot proposed chatHooks + in-process `registerLanguageModelChatProvider` (command hook = spawn = FORBIDDEN). Aider has no hook/middleware (litellm.completion direct). OS extension (DYLD_INSERT_LIBRARIES/path_helper/direnv/asdf/git hooks/launchd/systemd) is all new-process or env-only -> does not enter the model call, REJECTED.

**Security (Cluster B, deepen 0.937):** SO `state` trust boundary - only the user request enters. MRTR `InputRequiredResult.resultType=="input_required"` nested `systemPrompt`/`message` are server-sourced -> if they enter state, PROMPT INJECTION; the relay parses them out and removes them from the SO path, and the client produces an opaque AEAD `requestState` + a new id. Unknown `resultType` + generic Exception -> passthrough (NEVER 503); a real HTTPException blocks. secret-clean runs BEFORE COMBINING (e.g. `arguments.code` source code is masked). Capability gating is mandatory, otherwise -32602. `sampling`/`elicitation` are separate isolated calls, they do not change the main path. Rego/CEL forbidden -> plain `gate(layer0) bool` stdlib predicate, raw prompt/header does not enter the feature struct, log is boolean only.

**Calibration (Cluster C):** offline log set (no live SO), pure-Go isotonic PAV ~30 lines, ECE target 0.12->0.03, embedded static table (no DB). `strong_win_rate.quantile(1-strong_model_pct)` threshold; parse error/None -> strong pin (absence != low). If the R_zero baseline (convex hull AIQ) cannot be refreshed, revert to 0.8; conf-rule tied to the cache key. KS drift -> recalibration. References used: RouteLLM quantile, UCCE isotonic, FrugalGPT staged thresholds, RouterBench R_zero.

**Measurement (Cluster D):** no custom counters - standard `gen_ai.client.inference` span + `gen_ai.client.inference.usage.input_tokens`. Coverage = our own decision record's `_count` / all requests; rejections = `finish_reasons` content_filter/refusal. `exception.message` is PII -> never logged. There is no standard metric called `gen_ai.coverage.ratio` (it is our own record).

**Honest coverage ratio (item 30):** `(pre-model AND decision-authoritative AND relay-successful AND opt-in AND calibration-passing) / all requests` - intersection, NOT union. A client x authority matrix is kept; the real local scope is only OpenCode V2, others grow through opt-in adoption.

## 11. Checker inventory - 30 items, none missed (SO `ekle_ve_say` 0.957)

At least 1 verification per item to do. `VAR` = existing checker, `EKLE` = assert to be added to the existing 3 test files (no new file/module). Items 25-26 SKIP -> no checker.

| Item | To do | Checker | Status |
|---|---|---|---|
| 1 | single static binary | `CGO_ENABLED=0 go build ./...` | VAR |
| 2 | stdio relay core | `mcp_relay_test.go` frame round-trip | VAR |
| 3 | protocol fidelity | `mcp_relay_test.go` id/progressToken/protocolVersion copy | VAR |
| 4 | opt-in extension | `mcp_relay_test.go` com.kerem marker | VAR |
| 5 | route order | `route_test.go` Layer0/1/2 separate asserts | VAR |
| 6 | fail-safe | `route_test.go` conf 0.799->pass / 0.801->pin, timeout, HTTP!=200 | VAR |
| 7 | coverage ratio (not a guarantee) | shadow stderr distribution + rejection count report | VAR |
| 8 | 9 baked providers | `provider_test.go` (a) | VAR |
| 9 | providers.txt trust boundary | `provider_test.go` (d) key env hidden, (e) symlink rejection, (g) loopback Host | VAR |
| 10 | sticky pin + rotation | `provider_test.go` (c) no pin after restart, (f) deterministic rotation | VAR |
| 11 | SYSTEMONE_ROUTE_URL | `mcp_relay_test.go` env set->dedicated endpoint / unset->normal | KODDA |
| 12 | port fallback | `provider_test.go` (g) listen_addr loopback + port>0 | VAR |
| 13 | decision cache | determinism test + canary + counterfactual replay (§9) | VAR |
| 14 | decision evidence | counterfactual replay + per-phase counters | VAR |
| 15 | canary + self-heal | 1/N replay -> mismatch -> cell flush assert | VAR |
| 16 | 3-day shadow | uniqueness measurement + key/hit/margin logged side by side | VAR |
| 17 | SO schema | `route_test.go` questions object (NOT array) + choice/score + 1500ms/no retry | KODDA |
| 18 | tool selection | `route_test.go` allowlist/scope first + argmax assert | VAR (item 24) |
| 19 | stderr allowlist | `route_test.go` no secret/args/prompt/context/header/env/raw key, allowlist fields present | KODDA |
| 20 | health/fallback | `route_test.go` classify() transient/permanent -> reason-code | KODDA |
| 21 | implementation order | `CGO_ENABLED=0 go build ./... && go test ./...` (§6 step 6) | VAR |
| 22 | relay test | `mcp_relay_test.go` (item 22) | VAR |
| 23 | provider test | `provider_test.go` (a-g) | VAR |
| 24 | self-check requirement | every non-trivial logic -> assert; `go test ./...` | VAR |
| 25 | SKIP (HITL etc.) | no checker - deliberate | - |
| 26 | SKIP (self-spawn/network gate etc.) | no checker - deliberate | - |
| 27 | single-schema receipt | shadow JSON line + negative control (deliberately broken request -> rejection) | VAR |
| 28 | state trust boundary | `route_test.go` MRTR nested systemPrompt/message not in state + secret-clean first | KODDA |
| 29 | offline calibration | `route_test.go` PAV pre/post ECE assert (`ece_cal<ece_raw`) + R_zero comparison | KODDA |
| 30 | honest coverage ratio | `route_test.go` client x authority matrix: context-only/post-tool do not count | KODDA |

**Total: 28 checkers** (19 VAR + 9 EKLE->KODDA); 2 SKIP items have no checker. Distribution of the 9 EKLE: `route_test.go` 8, `mcp_relay_test.go` 1 - no new file/module, 503/fail-safe rules preserved.

**Implementation status (v1+):** all 28 checkers are met in code. `route.go`, `cache.go`, `guard.go`, `calibrate.go`, `transport_unix.go`, `transport_http.go` + related tests were added. `CGO_ENABLED=0 go build ./...` is clean. Final verification `CGO_ENABLED=0 go test ./...` (3 fixes after agent-lsp diagnostics: named-return panic recover, quantile epsilon, coalescing barrier).
