# System One router — Go daemon (`routerd`) planı

## YAPILACAKLAR — tek kontrol listesi (System One onaylı; G = m00559 hayatta kalma; H = m01041 router kullanımı)

> Bu liste planın tamamının özetidir. Uygulamada **hiçbir madde atlanmaz**; her madde aşağıda bir bölümde ayrıntılı. `SKIP` maddeleri bilinçli olarak yapılmaz — geri gelmez.

**A. Çekirdek / mimari (§0, §1d)**
1. Tek statik Go binary: `CGO_ENABLED=0`, stdlib only, harici dep yok. Config dosyası yok → `os.Getenv("SYSTEMONE_ROUTE_URL")`, `os.Getenv("NINE_ROUTER_LOG")`. Dağıtım `go install`/brew/tek indirme; client'a özel kod yok, sadece adres değişir.
2. Tek JSON-RPC 2.0 relay çekirdeği (`mcp_relay.go`); **v1: sadece `transport_stdio.go`**. 3. günde `transport_unix.go` + `transport_http.go` (Streamable HTTP, `MCP-Protocol-Version` tek yerde); SSE YOK.
3. Protokol sadakati: `id`, `_meta.progressToken`, `protocolVersion` birebir kopyalanır; sürüm client'tan alınır (hardcode yok); `sampling`/`roots`/`elicitation` yorumlanmadan geçirilir; **bilinmeyen capability → yorumlanmadan bayt-for-bayt geçir; sadece security/scope kararları deterministik reddedilir (sessiz yutma YOK anlamı: security kararı sessiz geçmez)**. Gerekçe: SEP-2577 roots/sampling/logging deprecated (go-sdk README onaylı); 3am on-call: bilinmeyen capability'ye "desteklemiyorum" demek client'ı bozar.
4. `capabilities.experimental['com.kerem/systemone-route'] = {enabled:false, mode:"relay"}` initialize'da ilan edilir (`schema.ts:310-314,386-390`, `additionalProperties:true`; üst seviyede `x-` YOK). SDK default-off → opt-in eden client route, etmeyen şeffaf relay. Çıktı = **kapsam oranı + ret sayısı**, garanti DEĞİL.
5. Route sırası: Katman 0 deterministik (normalize hash + statik kısa devre, µs) → Katman 1 System One (sadece AGENTS.md label-shaped kararlar, `conf>=0.8`) → Katman 2 şeffaf geçiş (varsayılan).
6. Fail-safe: `conf<0.8` / timeout / HTTP≠200 → şeffaf geçiş. Fail-closed (503) **varsayılan değil**. Önce 3 gün shadow mod (kararı çağır+logla, geçiş bozulmaz).
7. `before_model` MCP spec'te YOK → "her sistemde model-öncesi garanti" imkansız. Garanti iddiası yerine kapsam oranı raporlanır; ürün konumu "görünür kapsam ajanı".

**B. Provider registry / sıralama / port (§1e)**
8. Registry: compile-time `.rodata` 9'luk slice (OpenRouter, Ollama Cloud, OpenCode Free, v1m System One, Evren System One, Nace Drex, Ollama Local, Cloudflare, OpenCode Zen) + tek satırlık `providers.txt` (#10 slot: `ad, base URL, key env adı`). **Key dosyaya yazılmaz**, env'den gelir. Sıra = dosya satır sırası; dosya yoksa baked 9 sırası.
9. `providers.txt` trust boundary: `0600` + sabit path + **symlink reddi** (zorunlu trust boundary). Key env adı dosyada görünmez. Endpoint doğrulaması: **sadece loopback Host kontrolü** (loopback dışı Host → passthrough; DNS rebind koruması için gerekirse). Secret asla log'a.
10. Sıralama: **sticky pin** — son başarılı provider ID'sı process RAM'inde tek string (disk I/O YAGNI). Restart'ta pin yok → config sırası; ilk başarı RAM pin'i yazar. İstekte pin; ilk hatada pin düşüp `hash(sorted(reachable IDs))` sırasına geç. Saat/latency/health-probe **yok**; skor **yok**; her çağrıda System One **yok** (SO 0.937).
11. System One modelleri (v1m/Evren) için özel endpoint = `SYSTEMONE_ROUTE_URL` env. Set ise oraya yönlenir, aksi halde normal provider. Ayrı mutable katman / negotiation yok.
12. Port fallback: stdio/unix socket'te TCP portu hiç yok (çakışma imkansız). HTTP modu `127.0.0.1:0` → OS boş ephemeral port seçer; `listen_addr` zaten zorunlu stderr metric satırına yazılır. `MCP_PROXY_PORT` env ile sabit port override isteğe bağlı. **Port++ retry, SO_REUSEPORT, ayrı discovery dosyası YOK** (SO port oyu zayıf/çelişkili → kanıt + YAGNI kararı).

**C. Cache / karar maliyeti — ANA AMAÇ (§2, §9)**
13. **Karar cache'i VAR — ana amaç (pivot m00752/SO onay 0.980, §9):** 16 ADHD dalı + GPTCache/LiteLLM/ollaya kaynakları. **Cevabı değil SO'nun tipi kararını sakla** (label + conf bucket + rubric version) — düşük entropi, paraphrase'ler tek bucket'da birleşir. **Key = canonicalize(state) + rubricVersion + model + conf>=0.8 kuralı + registry version.** `state` key'e **girer**, ama canonicalize edilir (`encoding/json` sorted-key kopya + volatile alan ayıklama: timestamp/message-ref/tool-output/client adı/session ID). **Salt rubric/shablon yetmez** — SO etiketi state'e bağlı, rubric-only replay yanlış karar döndürür (deepen 1+3, sabotajcı `state tokenizer-spill` / `summary laundering`); eski 0-hit kök nedeni state'in *ham* key'de olmasıydı, canonicalize bunu çözer. Rubric/deploy değişince eski cell'ler deterministik **miss** olur (invalidation kodu yok). **Negatif cache:** `no`/`other`/`conf<0.8` de yazılır → tekrar eden belirsiz girdi 1500ms SO çağrısını atlar. **Sadece tipli label sakla, prose'u her hit'te yeniden üret** → bayat özet servis edilemez. **Giriş eşiği:** `conf>=0.8` VE probability margin ≥ eşik (clef overconfident 0.97–1.0 → tek yanlış label tüm client'lara yayılmasın). Hit = saf lokal okuma; SO çağrısı yok. **Miss'te in-flight coalescing (m00894/SO 0.888):** SO çağrı noktasına ~15 satır stdlib `Group{mu, m map[string]*call}` — aynı canonical key ile gelen 10 eşzamanlı istek 1×1500ms uçuşu paylaşır; `val`/`err` sadece `wg.Wait()` sonrası okunur, cleanup `if m[key]==c delete(...)` identity-check, lider panic→recover→passthrough, bir istemcinin `ctx.Done()`'u lideri zehirlemez, 1500ms aşan stale lider `Forget(key)`. `golang.org/x/sync` dahil YENİ MODÜL YOK (CGO=0 stdlib).
14. **Karar kanıtı:** oracle yok → "hit arttı" iddiası counterfactual replay (cache açık/kapalı aynı korpus) + per-faz sayaç (pre/lookup/admit/llm_save) ile ölçülür; cevap doğruluğu iddia edilmez. Her hit trace: `{key, decision_source, registry_version, conf, digest}`; secret/PII asla.
15. **Canary + self-heal:** bilinen etiketli sabit sorular her N çağrıda bir replay edilir; yanlış cached canary → cell flush + iç sayaç (pager yok). 1/N hit shadow'da yeniden SO'ya sorulup mismatch → cell flush (drift, zamana değil kanıta dayalı).
16. **3 gün shadow:** yalnızca key + hit/miss + margin yan yana loglanır, cevap değişmez. Hit adayı ölçülmeden cache servis açılmaz. (Ayrı disk store / mmap / Redis YOK — process-lifetime LRU `container/list`+map; restart→cold start kabul.)

**D. Güvenlik / gözlemlenebilirlik / sağlık (§3, §4)**
17. System One çağrısı: `POST https://evren-llmapi.ssyz.org.tr/v1/systemone`, `model:"clef"`, header `x-api-key` env'den. `questions` obje (array değil), sadece `choice`/`score`. `score` 0..1 float + `legend` + index-keyed `probabilities`. Timeout 1500ms, **retry yok**, `conf<0.8` → None/fail-safe. `state` her şeyi içerir ama secret/PII/uzun context **asla**; untrusted içerik ayrı alan. `localhost:20128/...` varyantı kullanılmaz.
18. Araç seçimi + parametre doğrulama: allowlist/scope **önce**, deterministik; JSON Schema ~20 satır (type/required/enum/properties/items); tek aday direkt, çok aday + çözülmedi → System One `choice`. **System One asla izin/scope kararı vermez.** `state`'e tool çıktısı / ham kullanıcı metni girmez. `conf>=0.8 && argmax`, retry 1 (6sn), sonra deny/ask.
19. Gözlemlenebilirlik (`metric.go`): stderr'a **1 JSON satırı**, allowlist `tool/route/miss_reason/decision/model + ts/dur_ms + key_hash(12 hex) + listen_addr + seq/pid + reason_code/covered/abstained/passthrough`. `conf`/`tokens`/`cost` **allowlist dışı** — doğrulanamadı, kendi sürecinin iddiası, shadow'da oracle yok. **Yasak:** args, prompt, context, header, env, ham key, secret. `jq -s` raporu; `gate_called` oranı ve kapsam oranı + ret sayısı raporlanır.
20. Sağlık/fallback (`guard.go`): `classify(status,exc)` transient `408/429/500/502/503/504`+timeout, permanent `400/401/403/404/422` — passthrough reason-code için KALIR. Tek bool `so_degraded`: bir kez timeout/HTTP≠200 → passthrough, tekrar deneme yok. Breaker / state makinesi / `health.json` SİL (dosya I/O YAGNI). Backup aynı :20128 ise failover anlamsız (correlated failure).

**E. Test / uygulama sırası (§6)**
21. Sıra: shadow mod (3 gün) → **v1 stdio** → MCP relay testi → provider testi → `CGO_ENABLED=0 go build ./... && go test ./...`. 3. günde: benzersizlik ölçümü + unix + Streamable HTTP (SSE YOK).
22. `mcp_relay_test.go`: extension'sız client `initialize` + tool call tamamlar; extension'lı client `com.kerem/systemone-route` marker'ını görür; `id`/`progressToken`/`protocolVersion` korunur.
23. `provider_test.go` (7 madde): (a) `providers.txt` yoksa 9 baked sıra, (b) tek satır → #10, (c) restart sonrası pin yok → config sırası, (d) key env adı dosyada görünmez, (e) symlink reddi, (f) ilk hatada deterministik rotation, (g) HTTP `listen_addr` loopback + port>0 + stderr'da secret yok.
24. Her yeni non-trivial logic için assert-based self-check; `route_test.go` dahil tüm testler `go test ./...` içinde geçmeli.

**F. SKIP — bilinçli olarak YAPILMAZ (geri gelmez)**
25. HITL yeni kodu (walsy-reflex v0.4.0 zaten %80; sadece 4 hard kategori `policy.yaml`'a eklenir), oturum belleği, rate limit/kota, multi-provider ortak `generate()` arayüzü, ön-filtre ayrı `.mjs` dosyası.
26. 9router patch / `semanticCacheEntries` katmanı, self-spawn (SO 0.880), ağ kapısı/DNS+307 (SO 0.008), LD_PRELOAD/eBPF, embedding/vektör DB, yeni MCP sunucusu, provider/cache katmanı fork'u, tüm `.py`/Rust yolları, lrecon (PARK, §7). Hayatta kalma (m00559): self-restart, global lock, kist/epoch kalıcılığı, torpor sonrası cache-karar otomasyonu, quorum/dış sinyal, yeni daemon, senkron disk makbuzu. Hepsi madde 6'nın tek invariant'ına düşer — ayrı kod YOK.

**G. Kanıt (§8)**
27. **Tek şema — ayrı makbuz DOSYASI YOK:** v1 canlı = stderr tek reason-code satırı. 3 gün shadow = stderr append-only JSON line `{seq,pid,ts,decision,reason_code,covered,abstained,passthrough,latency_ms}`. `seq` boşluğu = drop/restart ayırımı için `pid` (ayrı dosya yok). Negatif kontrol: 3 gün shadow'da kasıtlı bozuk istek → ret gelmeli; ret yoksa tur geçersiz. Satır yoksa = gated edilmemiş. `conf`/`tokens`/`cost` doğrulanamadı (kendi sürecinin iddiası, shadow'da oracle yok) — allowlist dışı.

**H. Router kapsamı / güvenlik / kalibrasyon (§10, m01041 SO 0.937)**
28. **SO `state` = trust boundary:** sadece kullanıcı isteği girer; MRTR `input_required` nested `systemPrompt`/`message` asla girmez (prompt injection). `secret-clean` birleştirmeden ÖNCE. Capability gating yoksa -32602. Bilinmeyen `resultType` + generic Exception → passthrough; 503 ASLA.
29. **Kalibrasyon (offline, canlı SO YOK):** saf-Go isotonic PAV + ECE hedefi 0.12→0.03; sabit 0.8 yerine `quantile(1-strong_model_pct)`; parse hatası → strong pin (yokluk ≠ düşük); R_zero baseline yenilemezse 0.8'e dön; `conf-rule` cache key'e bağlı. Rego/CEL YOK → düz `gate(layer0) bool`.
30. **Dürüst kapsam oranı:** `(model-öncesi ∧ karar yetkili ∧ relay başarılı ∧ opt-in ∧ kalibre geçen) / tüm istekler` — birleşim DEĞİL kesişim. Client×otorite matrisi tutulur; yerel gerçek kapsam sadece OpenCode V2, diğerleri opt-in benimsemeyle büyür.

> Durum: hayatta kalma simülasyonu (m00559, 8 ADHD ajanı) tamam → §8 + G maddeleri eklendi (SO `complete` p=0.627). 17 router dalı + 5 ADHD frame + 2 deepen + router audit tamam. System One mimari oylaması tur 2: **go_routerd_plus_ts_shim 0.939** (prob 0.954, ≥0.8 = geçerli verdict). Tur 1 zayıf seçimi (saf standalone proxy 0.731) override edildi.
> MCP-global araştırması (m00251): 5 frame + 4 deepen + web kanıt. System One: **koru_ve_mcp_extension_ekle 0.951** (prob 0.963); kullanıcı iddiası **kısmen doğru** (score 1.05, p=0.882). Aşağı §1b.
> **GLOBAL PIVOT (m00377, SO 0.961/0.984):** 9router/opencode'a özel port YOK. Tek statik Go binary = protokol-seviyesi global MCP relay; 9router/opencode sadece örnek dağıtım. **Aşağı §0 ve §1d güncel — önceki §0 metni tarihseldir.**
> Repo: `/Users/keremurhan/walsy-system1` — greenfield, 0 dosya. Secret'lar asla plan'a/log'a yazılmaz.
> Kapsam (m00181): **System One router only.** lrecon = analoji, PARK (aşağı §7).

## 0. Nihai mimari — GLOBAL MCP relay (System One onaylı, m00377)

Tarihsel not: burada eskiden "OpenCode → TS shim → Go routerd → 9router" yazıyordu. Kullanıcı (m00377) 9router/opencode'a özel port istemiyor; System One pivot **global_relay_arastir 0.969** onayladı, mimari oylaması **global_relay_core 0.961**, revize **0.984**.

```
herhangi MCP client  ─┐
                      ├─→ [tek statik Go binary: JSON-RPC 2.0 relay çekirdeği] ─→ MCP server
herhangi MCP server  ─┘         ↑ KARAR (opsiyonel, opt-in)
```

- **Tek statik Go binary** — `CGO_ENABLED=0`, libc yok, harici dep yok. `go install` / Homebrew / tek dosya indir = aynı davranış. Config dosyası YOK: `os.Getenv("SYSTEMONE_ROUTE_URL")`, `os.Getenv("NINE_ROUTER_LOG")`.
- **TEK röle çekirdeği, taşıma aşamalı:** **v1 sadece (1) stdio child (`type:local`)**. 3. günde (2) Streamable HTTP (`type:remote`; SSE YOK) + (3) unix socket/FIFO dosya modu. Client tarafında özel kod YOK — sadece config'de adres (komut/URL/dosya yolu) değişir. 9router/opencode **örnek dağıtımdır**, kod yolu değil.
- **Protokol sadakati:** `id`, `_meta.progressToken`, `protocolVersion` birebir korunur; HTTP'de `MCP-Protocol-Version` header zorunlu; `sampling`/`roots`/`elicitation` server→client geri-istekleri şeffaf geçer; **bilinmeyen capability → yorumlanmadan bayt-for-bayt geçir; sadece security/scope kararları deterministik reddedilir (sessiz yutma YOK anlamı: security kararı sessiz geçmez)**. Gerekçe: SEP-2577 roots/sampling/logging deprecated (go-sdk README onaylı); 3am on-call: bilinmeyen capability'ye "desteklemiyorum" demek client'ı bozar.
- **Kapsam mekanizması:** initialize'da `capabilities.experimental['com.kerem/systemone-route'] = {enabled:false, mode:"relay"}` ilan edilir (schema.ts:310-314,386-390, `additionalProperties:true`; üst seviyede `x-` YOK). SDK'lar default-off: opt-in eden client'larda route aktif, etmeyenlerde **şeffaf relay**. Çıktı = **garanti DEĞİL, kapsam oranı + ret sayısı**.
- **MCP spec'te `before_model` YOK** — model çağrısı MCP dışında. "Her sistemde model-öncesi garanti" imkansız (System One p=0.718). Karar yalnızca MCP trafiği içinde, gerçek LLM garantisi `:20128` zorunlu geçişte (opsiyonel gözlem: `proxy.log` + `route_counts`).
- **Route sırası:** Katman 0 deterministik (normalize hash + statik kısa devre, ~µs) → Katman 1 System One (sadece AGENTS.md label-shaped kararlar, conf≥0.8) → Katman 2 şeffaf geçiş (varsayılan).
- **Fail-safe:** `conf<0.8` / timeout / HTTP≠200 → şeffaf geçiş. Fail-closed (503) varsayılan değil — relay ölürse client MCP'siz kalır; önce shadow mod (kararı çağır+logla, geçiş bozulmaz).
- **Self-spawn YOK** (m00376 SO 0.880), **ağ kapısı YOK** (0.008), **LD_PRELOAD/eBPF YOK** (imzalı binary'de kırılır / root+Linux'e kilitli = "her sistem" değil).
- Gereksiz System One çağrısı = vergi (~0.5–1.7s + 350 tok). Her isteğe sorulmaz.

## 1. ADHD Phase 2 — 5 frame × 6 fikir + 2 deepen

Kırılgan varsayım: "Go = MCP server", "modelden önce = client hook", "plugin TS olmak zorunda", "ayrı process", "request-response". Hiçbiri zorunlu değil; karar katmanı HTTP'de, tetikleyici plugin'de.

**Küme A — araya girme noktasını taşı**
- HTTP middleware hard gate (model atlayamaz) `[N8 V9 F10]` ★
- Go binary = 9router'ın ta kendisi `[N9 V6 F10]` — *tuzak: patch kırılgan*
- provider istek kuyruğu = "önce" `[N9 V6 F9]`
- fail-closed 503 `[N8 V7 F9]` — *tuzak: SPOF*
- tek uzun stream, pipeline karar `[N7 V5 F7]`
- TS plugin hiç yazılmaz `[N6 V3 F8]` — *tuzak: invocation zorunlu*

**Küme B — karar maliyetini yok et**
- singleflight: aynı state 99 kopya → 1 çağrı, fan-out `[N8 V9 F9]` ★
- deterministik kısa devre (/models, statik, retry) `[N7 V9 F10]` ★
- karar tablosu tek cache-line (8-byte key, atomik compare) `[N8 V8 F8]`
- branch predictor: son N karardan örüntü, tekrarda gate yok `[N8 V7 F9]`
- tek thread + ring buffer (spawn/GC yok) `[N7 V7 F7]`
- sabit 350-token prefix → clef cache ~100% `[N9 V6 F8]` — *tuzak: prefix garanti değil*
- speculative execution `[N9 V5 F7]` — *tuzak: model zaten başladı*
- CPU pin / fork-server `[N6–7 V5–6 F6–7]`
- byte glitch (1 karakter kırp, Jaccard 0.999) `[N8 V4 F6]` — *tuzak: yanlış rota*

**Küme C — garantiyi gözleme çevir (3am)**
- shadow router (log-only, yönlendirmez) `[N8 V9 F5]`
- config binary'ye göm (disk dosyası yok) `[N7 V9 F7]`
- self-shutdown → trafik doğrudan main `[N8 V8 F6]`
- tamamen stateless (health.json/breaker yok) `[N7 V8 F6]`
- sessizlik kontratı `[N9 V8 F4]`
- bypass header `[N7 V8 F8]`

**Küme D — zehirlenmeye karşı**
- state untrusted alan, talimatlar ayrı alan (injection kırılır) `[N7 V9 F9]`
- `Secret` tipi + redacted `String()`; key stdin/exec, env/argüman yok `[N6 V9 F8]`
- şema binary'de, config override edemez; her yanıta sürüm damgası `[N7 V8 F8]`
- ters akış / namespace slip / clef kendisi karar üretsin — **hepsi tuzak** (garanti yanılsaması)

**Yakınsama:** Ayanlanmış ama sıradan: tek cache-line tablo, shadow router. **Non-obvious-but-viable ★: Go karar + 10 satır TS tetikleyici + Küme B/C sentezi** — garanti mimari katmanda, maliyet tekrarlarda sıfır, SPOF shadow modla yönetiliyor.

**Derinleştirme 1 — HTTP-middleware hard gate (fail-closed):**
9router :20128 tüm trafiği görüyor; karar MCP/plugin katmanından HTTP katmanına taşınır. İstek → System One `/v1/systemone` (clef) → "devam" ise upstream, "red"/yanıt yoksa 503. Çift oyu önlemek için mevcut plugin oyu kaldırılır, tek karar kaynağı daemon. Ama her isteğe ~350 tok + gecikme bindiği için çoğu istek çağrısız geçmeli. Daemon tek hata noktası → önce shadow mod. *Load-bearing risk:* SPOF (System One 0.816). *İlk adım :* shadow modda middleware ekle, kararı logla, 503 döndürme (güven 0.985). *Alt fikirler:* sağlık kontrolü + önbelleğe alınmış son kararla geçiş; cache-first TTL map; bypass header; plugin sadece metrik okusun; shadow log şeması (etiket/güven/gecikme/hash).

**Derinleştirme 2 — cache-only hard gate (deterministik kısa devre + sabit prefix + singleflight):**
İstek yakalanır → normalize hash → L1 atomik karar (~µs) → bilinen statik/tekrar isteği cache'den, hiç model çağrılmaz → gerçek label-shaped yeni istek → singleflight ile tek goroutine System One'a, 99 kopya fan-out → conf<0.8/timeout → main. *Load-bearing risk:* **state cache key'inde olmak zorunda → benzersiz isteklerde hit ~0.** "Near-zero System One çağrısı" sadece retry/restart/fan-out tekrarlarında tutar; state düşürülürse sessiz yanlış rota. İkinci kırılma: in-path olmadan garanti yok, fail-open olunca da garanti yok. *İlk adım :* 9router access log'undan son 1000 isteğin `sha256(state)` benzersizlik oranını say; >%95 ise L1 yazma, doğrudan singleflight'e geç (20 satırlık script, daemon kurmadan karar verir). *Alt fikirler:* L1 sadece retry'lerde; derleme-zamanı karar tablosu (Go switch/enum); sidecar UDS + plugin socket dener; singleflight'i `(state, questions)` çiftine göre 100ms coalescing ile anahtarla, L1 kaldır; 1 hafta sadece telemetri ("hit olsaydı" sahte hesabı), kısa devre yok.

**Provokasyon:** "Garantili model-öncesi" ile "her istekte System One" aynı şey mi? Hayır — garanti = *her isteğin daemon'dan geçmesi*, System One çağrısı = *sadece gerekliyede*. Bu ayrımı netleştirmeden hiçbir cache/dispeç kodu yazılmaz.

## 1b. MCP-global proxy araştırması (m00251, System One onaylı)

**Kullanıcı iddiası:** "MCP proxy olarak yazılırsa daha global ve herkese uygun olur, 9router/OpenCode'a özel yazılmaz." → System One: **kısmen doğru** (score 1.05/2, p=0.882). Doğru olan: MCP'nin transport-agnostik JSON-RPC 2.0 katmanı gerçekten client-agnostik. Yanlış olan: bu katman *model-öncesi garanti* veremez.

**Neden veremez (spec kanıtı):** MCP 2025-11-25'te `before_model` fazı YOK. Model çağrısı MCP'nin dışında, client'ın kafasında. Server sadece araç sunar; çağırıp çağırmamayı model seçer. `sampling`/`roots`/`elicitation` server→client geri isteklerdir — proxy geçirir ama zorunlu kılamaz. Bilinmeyen capability bayt-for-bayt geçirilir; sadece security/scope kararları deterministik reddedilir (sessiz yutma YOK anlamı: security kararı sessiz geçmez).

**Karar (SO 0.951):** Mevcut §0 mimarisi KORUNUR (Go routerd karar + TS shim tetikleyici). Üstüne client-agnostik bir MCP JSON-RPC relay eklenir — **tam global proxy değil**, veri düzlemi global / tetikleyici düzlemi client-specific.

```
DATA plane  → Go MCP relay (v1 stdio; 3. gün Streamable HTTP, spec-sadık) — client-agnostik
TRIGGER plane→ mevcut 10 satır TS shim (OpenCode) — client-specific, garanti BURADA
ÖLÇÜM       → "kapsam oranı": kaç client extension'ı açtı, loglanır
```

**Spec-yasal extension (webfetch doğrulandı):**
- MCP extensions resmi mekanizma: SEP-2133 (Final) + `https://modelcontextprotocol.io/extensions/overview`.
- ID formatı `{vendor-prefix}/{extension-name}`, ters domain zorunlu öneri → `com.kerem/systemone-route` (ad alanı sahipliği varsayımı; kurumsal değilse değiştir).
- Yerleşim: `capabilities.experimental` altına — `schema.ts:310-314,386-390` → `experimental?: {[key:string]:object}`, `additionalProperties:true`. **Üst-seviyede `x-` alanı YOK**; strict validator reddedebilir.
- SDK'lar extension'ları **default OFF + explicit opt-in** uygular; extension desteği %100 protokol konformitesi için GEREKLİ DEĞİL → desteklemeyen client initialize'ı reddetmez, görmezden gelir. "Herkes bağlanabilir" doğru; "herkes route edilir" yanlış.
- `before_model` yine yok → extension yalnızca discovery/kapsam taşır, **enforcement sıfır**. Garanti TS shim'de kalır. Yeni capability negotiation field isteyen her şey SEP gerektirir (`/community/sep-guidelines`).
- Relay kuralları: `initialize` protocolVersion'ı client'tan al (hardcode yok, HTTP'de sonraki isteklerde `MCP-Protocol-Version` header zorunlu); JSON-RPC `id` + `_meta.progressToken` birebir kopyala; `sampling`/`roots`/`elicitation` yorumlamadan geçir; **bilinmeyen capability → yorumlanmadan bayt-for-bayt geçir; sadece security/scope kararları deterministik reddedilir (sessiz yutma YOK anlamı: security kararı sessiz geçmez)**. Gerekçe: SEP-2577 roots/sampling/logging deprecated (go-sdk README onaylı); 3am on-call: bilinmeyen capability'ye "desteklemiyorum" demek client'ı bozar.
- Kaynaklar: `/extensions/overview`, `/seps/2133-extensions`, `/specification/2025-11-25/basic/lifecycle`, `/community/sep-guidelines`, `schema.ts` (2025-11-25).

**Tuzaklar (reddedildi):** tam global proxy (SO 0.013 — teknik olarak imkansız); ağ kapısı/DNS+307 (SO 0.008 — DoH/DoT kaçağı, SPOF, MITM CA, SSE koparır); `x-systemone-route` üst-seviye alan (spec'te yok); "spec-yasal = garanti çözüldü" yanılsaması.

**Relay self-check:** extension'sız bir client'ın `initialize` + tool call'u sorunsuz tamamladığını kanıtlayan tek kabul testi; extension'lı client'da `com.kerem/systemone-route` marker'ının `_meta`/`capabilities.experimental` içinde görüldüğünü assert et. Bu test olmadan "herkese uygun" iddiası retoriktir.

## 1c. Self-spawn proxy araştırması (m00326, System One onaylı)

**Kullanıcı sorusu:** "MCP çalışırken kendi proxy'sini açsa, paketlenen lrecon gibi Go binary'sini çalıştırsa, her isteği otomatik algılasa — mümkün mü?" (lrecon yalnızca analoji, PARK, kapsam dışı.)

**ADHD:** 5 frame × 6 fikir (10-year-old, game design, ant colony, $0/1h, attacker) + 3 deepen.

**Mekanik cevap: MÜMKÜN, AMA YANLIŞ KAPSAM.** `go:embed` ile binary'yi ilk MCP çağrısında 0700 `mkdtemp` dizine yazıp `exec` etmek teknik olarak çalışır. Ebeveyn PID canlılığı + soy zinciri + inode/SHA-256 sabitleme + HMAC kanal + tek kullanımlık nonce eklenebilir. **Fakat** bu proxy yalnızca MCP stdio'sundan geçen trafiği görür; model HTTP çağrıları `:20128` üzerinden MCP **dışında** gider → "her isteği otomatik algılama" **GARANTİSİ YOK**, güvenlik tiyatrosu.

**System One kararı:** `koru_self_spawn_yok` **0.880** (prob 0.920) — self-spawn **EKLEME**, mevcut §0 mimarisi korunur. İddia `kısmen` doğru (argmax p=0.767, conf 0.651 zayıf → YAGNI + spec kanıtıyla sabitlendi).

| Seçenek | SO olasılığı | Sonuç |
|---|---|---|
| `koru_self_spawn_yok` | 0.920 | **KABUL** — mevcut daemon + relay yeterli |
| `sadece_tee_gozlem` | 0.064 | reddedildi — MCP trafiğini ölçer, LLM trafiğini ölçmez, ek kod |
| `ekle_self_spawn` | 0.017 | reddedildi — aynı kapsamı daha pahalı + SPOF + saldırı yüzeyi |

**Gerçek "her LLM isteği" kaynağı:** `:20128` 9router zorunlu geçişi (headroom `proxy.log` + `route_counts`), zaten daima çalışan launchd daemon'unda. Yeni process açmaya gerek yok.

**Tuzaklar (reddedildi):**
- Self-spawn'ı "her isteği yakalar" sanmak = kapsam yanılsaması.
- Soy-ağacı kriptografisi = yükü taşımayan yeni saldırı yüzeyi.
- inode/hash sabitlemesini `exec` **sonrasına** koymak = geç koruma; extract dizini öngörülebilir = PATH/host hijack.
- Kapsam oranını garanti gibi sunmak = sessiz istekleri gizler; ret sayısı açıkça raporlanmalı.

**Eklenen görev yok:** `mcp_relay.go` / TS shim zaten §1b'de. Self-spawn için yeni dosya, yeni process, yeni bağımlılık **yok**. İstersen tek gözlem: stdio JSON-RPC satır sayacını `proxy.log route_counts` ile karşılaştırıp kapsam boşluğunu sayısal raporla (ölçüm, kod değil).

## 1d. Global MCP proxy kararı (m00377, System One onaylı)

**Kullanıcı:** "9router+opencode portu yapmacaksın — bu global bir MCP proxy olacak, her sistemde çalışan, bağımlılığı pek fazla olmayan." → Önceki 9router/opencode'a özel daemon+shim anlatımı **geçersiz**, §0 güncellendi.

**System One turları:**
| Tur | Karar | Sonuç |
|---|---|---|
| Pivot (m00382) | `global_relay_arastir` **0.969** (p 0.980) | Kabul — özel port yok, global relay ara |
| Pivot (m00382) | "her sistemde model-öncesi garanti" `imkansız` p=0.718 (conf 0.516 zayıf) | Yön net: garanti yerine kapsam oranı |
| Mimari (m00413) | `global_relay_core` **0.961** (p 0.974) | Tek binary + 3 taşıma + opt-in |
| Revize (m00413) | `evet_revize` **0.984** (p 0.992) | plan.md §0/§1d güncellendi |

**ADHD:** 5 frame × 6 (remove-assumption, $0/1h, logistics, 3am, biology) + 3 deepen (taşıma/paketleme/kapsam).

**Kümeler (N.35+V.40+F.25, tuzaklar hariç):**
- **Taşıma katmanını yok et:** FIFO/unix socket dosya modu `[N7 V8 F9]`, soket aktivasyonu (launchd/systemd fd handoff) `[N8 V7 F8]`. Tuzak: LD_PRELOAD/DYLD `[N9 V5 F8]` imzalı binary'de kırılır; eBPF `[N9 V3 F7]` root+Linux'e kilitli → "her sistem" yalan.
- **Tek binary, sıfır config:** `argv[0]` ile çok kimlik `[N8 V9 F9]`, `CGO_ENABLED=0` statik binary `[N6 V9 F9]`. Tuzak: c-shared dylib LD_PRELOAD bağımlığı geri getirir.
- **Protokol meşruiyeti / kapsam:** `capabilities.experimental` opt-in + şeffaf relay fallback `[N7 V9 F10]` ★, roots ile cwd→proje kökü `[N8 V7 F8]`, kapsam oranı + ret sayısı `[N6 V9 F9]`.
- **Kendini onarım (3am):** MCP `ping` kalp atışı + capability re-negotiation `[N8 V8 F8]`, `progressToken` ile durum taşıma (DB yok) `[N7 V8 F7]`, `resources/list` URI'sine health kodlama `[N6 V8 F6]`. Tuzak: `sampling/createMessage` ile host LLM'ini router sanmak — geri-istek refusability taşır, garanti değil.
- **Biyoloji (wild):** anerji (sessiz kalıp uyanma), quorum eşiği, moleküler mimikri, red-queen, seyrek kodlama, plasmid yetenek devri — hepsi **manipülasyon** kokuyor, reddedildi.

**Converge (★ = non-obvious ama viable):**
1. ★ **Global relay core** — tek JSON-RPC çekirdek + 3 taşıma adaptörü, client-agnostik, bağımlılık sıfır. SO 0.961.
2. ★ **`capabilities.experimental` opt-in** — "her sistemde çalışır" iddiasını *"her sistemde en azından şeffaf geçer, destekleyenlerde yönlendirme yapar"* seviyesine indirir. Garanti değil kapsam oranı.
3. **Statik tek binary + env config** — dosya/config katmanı yok; `go install`/brew/tek indirme. Risk: env değişince daemon yeniden başlamaz (restart bağımlılığı), HTTP remote için allowlist gerekir.

**Tuzaklar (ayrı, red gerekçesiyle):**
- Self-spawn geri girişi — m00376 SO 0.880 ile reddedildi (kapsam yanılsaması).
- "Model-öncesi garanti" — MCP spec'te `before_model` yok, imkansız.
- Ağ kapısı / DNS/307 — SO 0.008; DoH kaçağı, SPOF, MITM CA.
- Kapsam oranını garanti gibi sunmak — sessiz istekleri gizler; ret sayısı açık raporlanmalı.
- eBPF/LD_PRELOAD — "her sistem" iddiasını kırar.

**Focus — 3 deepen özeti:**
- **Taşıma:** `ReadFrame/WriteFrame` tek sözleşme; **v1 sadece stdio**; 3. günde unix socket aynı implementasyonu paylaşır + Streamable HTTP (MCP-Protocol-Version tek yerde; SSE YOK). Risk: kararın uygulandığı doğrulanamaz (model MCP dışında) + SDK default-off sessiz devre dışı.
- **Paketleme:** `CGO_ENABLED=0` + `os.Getenv` tek init; `mcp_relay_test.go` env set/unset assert. Risk: self-spawn tuzağı, env restart bağımlılığı, HTTP remote allowlist.
- **Kapsam:** initialize'da `{enabled:false, mode:"relay"}` örnek payload; client opt-in olmadan alan görmezden gelinir. Risk: ilk gün kapsam oranı ~0 olabilir, ölçüm yoksa ret sayısı görünmez.

**Provokasyon:** Eğer garanti imkansızsa, ürünün adı "router" değil "gözlemlenebilir kapsam ajanı" mı olmalı? Kullanıcı hangi vaadi satın alıyor: yönlendirme mi, görünürlük mü?

## 1e. Provider registry / sıralama / port fallback (m00445, 16 ajan)

**Kullanıcı (m00445, Türkçe özü):** MCP proxy localhost'ta çalışsın, 9router gibi küçük bir System One çağırma yeri olsun; provider ekleyip **sıralamak**; System One modelleri (v1m/Evren) için özel localhost endpoint; dolu port → otomatik boş port fallback. Hazır 9 provider: OpenRouter(Connected), Ollama Cloud(none), OpenCode Free(Ready), v1m System One(none), Evren(System One)(Connected), Nace Drex, Ollama Local, Cloudflare, OpenCode Zen + bireysel ekleme slotu.

**ADHD:** 13 diverge × 6 fikir + 3 deepen = 16 ajan (research-only). Skor kümesi: taşıma/registration güvenliği N.33+V.38+F.30, skor/SO mekanizmaları N.10+V.4+F.3.

**System One turları (şeffaf kayıt):**
| Soru | Tur 1 | Tur 2 (sıkı state + güvenlik kontrolleri) | Karar |
|---|---|---|---|
| Paket kabulü | `kismi_kabul` 0.274 (zayıf) | `tumu_kabul` 0.659, p=0.773 (**zayıf**, <0.8) | Ranking+registry kabul; port ayrı |
| Sıralama | `sticky_pin_rotation` 0.937, p=0.958 | tekrar sorulmadı | **NET — kabul** |
| Port | `bind0_discovery` 0.740 (zayıf) | `mevcut_tasima` 0.299, p=0.533 (**zayıf + çelişkili**) | **Yeni port kodu YOK** |

> Zayıf verdict kuralı: irreversible değil → bir kez sıkı state ile retry yapıldı; hâlâ <0.8 ve çelişkili olduğu için port konusunda **kanıt + YAGNI** ile karar verildi, model oyu görüntülenmedi. Ranking (0.937) tek güçlü verdict.

**Kabul edilen mekanizmalar:**
1. **Sıralama = sticky pin + deterministik rotation — skor YOK, System One YOK.** Tek kanıt biti: son başarılı provider ID'sı process RAM'inde tek string (disk I/O YAGNI). İstekte pin kullanılır; ilk hatada pin düşürülüp `hash(sorted(reachable IDs))` sırasına göre bir sonraki provider'a geçilir. Saat/latency/health-probe yok. Restart'ta pin yok → config sırası; ilk başarı RAM pin'i yazar. Pheromone / tick-size / margin-collateral / statik-priority / "her çağrıda SO" = YAGNI — zayıf 0.51 skorunu başka biçimde yeniden üretirler (SO 0.937 bunu doğruladı).
2. **Registry = compile-time `.rodata` 9'luk slice + tek satırlık `providers.txt` (#10 slot).** Satır 3 alan: `ad, base URL, key env adı` — **key dosyaya yazılmaz**, env'den gelir (credential tek kaynak). Sıra = dosya satır sırası, dosya yoksa baked 9 sırası; reorder = satır taşıma, kod değişikliği yok. Yeni MCP mutation protokolü YOK.
3. **System One için özel endpoint = `SYSTEMONE_ROUTE_URL` env.** Set ise v1m/Evren System One modelleri oraya yönlenir; aksi halde normal provider. Ayrı mutable katman/negotiation YOK (kısıt zaten capability negotiation'ı dışlıyor).

**Güvenlik (trust boundary — atlanamaz):**
- `providers.txt`: `0600` + sabit path + **symlink reddi**. Yerel attacker satır yazarsa trafiği kendi URL'ine yönlendirir / key env adı değiştirip credential exfil zemini hazırlar.
- Pin: process RAM'inde tek string (disk I/O YAGNI; restart'ta config sırası, ilk başarı RAM pin'i yazar).
- Endpoint doğrulaması: **sadece loopback Host kontrolü** (loopback dışı Host → passthrough; DNS rebind koruması için gerekirse). Secret asla log'a.

**Port fallback kararı (kanıt + YAGNI) — 3. güne ertelendi (v1 sadece stdio):** `Port++` retry döngüsü, `SO_REUSEPORT` ve ayrı discovery dosyası **yok**. Kullanıcı isteği "dolu port → otomatik boş port" şu şekilde karşılanıyor:
- **stdio / unix socket:** TCP portu hiç yok — çakışma kavramsal olarak imkansız.
- **HTTP modu (3. gün):** `transport_http.go` sabit port yerine `127.0.0.1:0` ile `net.Listen` çağırır; **OS o an boş ephemeral port'u seçer** — bu, "otomatik boş port"un ta kendisi, tarama/retry kodu yok. Seçilen adres, zaten zorunlu olan `metric.go` stderr JSON satırına `listen_addr` alanı olarak yazılır (yeni trust boundary değil — aynı allowlist kanal); client/supervisor bu satırdan bağlanır. `getsockname()` sonucu tek satır, secret-free. Sabit port tercih edilirse `MCP_PROXY_PORT` env ile override edilebilir (o zaman çakışma kullanıcı tercihidir, retry yok).

> Bu karar SO port oylarının **zayıf/çelişkili** olması nedeniyle kanıt + YAGNI ile verildi (tur 1 `bind0_discovery` 0.740, tur 2 `mevcut_tasima` 0.299/p=0.533). Ayrı discovery dosyası yerine mevcut stderr kanalı kullanıldı; dolayısıyla tur 2 argmax'ı (`mevcut_tasima`) ile yeni kod arasındaki çelişki minimize edildi.

**Load-bearing risk:** Pin, hata vermeyen ama yavaş (degraded) provider'a takılabilir — rotate tetiklenmez; "başarı" tanımı (2xx yeterli mi, latency eşiği mi) netleştirilmeli. İlk gün kapsam oranı ~0 kalabilir (§1d).

**Kabul testi (`provider_test.go`):** (a) `providers.txt` yoksa 9'luk baked sıra, (b) tek satır eklenince #10 olur, (c) restart sonrası pin yok → config sırası, (d) key env adı dosyada görünmez, (e) symlink reddedilir, (f) ilk hatada rotation deterministik ve aynı state'de aynı sıra, (g) HTTP `listen_addr` loopback + port>0 ve stderr metric satırında secret yok.

## 2. Cache gerçeği (doğrulandı — salt okuma)

- **9router `semanticCacheEntries`: ÖLÜ ŞEMA.** Tablo var (key/model/responseBody/hitCount/tokensSaved + 2 index), `count(*)=0`, `~/.9router/runtime/**/*.js` içinde **0 referans**. Katman 0 bunu kullanamaz; yama yazılmaz.
- **Headroom `semantic_cache.py`: kod aktif, 0 hit.** PID 50029 = `python -m headroom.cli proxy --port 8180 --code-aware`; `proxy/server.py:1196-1203` cache instance var, defaults `cache_enabled=True`/TTL 3600/LRU 1000; `/health` healthy. Ama `/metrics` → `headroom_requests_cached_total 0`, PERF log → `cache_read=0 cache_write=0 cache_hit_pct=0`. Sebep: handler'larda `not stream` koşulu (`handlers/anthropic.py:1445`, `handlers/openai.py:3930`) + per-process HMAC partition. Process-içi `OrderedDict` → router'dan HTTP ile okunamaz.
- **Log'daki `cache_hit`/`cache_miss` başka katman:** `headroom/transforms/content_router.py:1586-1598` (lokal transform cache, ~54µs, 79 hit/1 miss). `cross_turn_dedup` da orada. `semantic_cache.py` ile ilgisi yok. `headroom.content_router`'a dokunulmaz.
- **Sonuç (madde 13 pivot, §9):** Router **kendi karar cache'ini kurar** — iki mevcut cache'e bağlanılmaz (ölü şema / 0 hit). Eski process-içi `cache_store` kendi kendini doğrulayan metrik üretiyordu; yeni tasarım bunu **counterfactual replay + per-faz sayaç** ile düzeltir (madde 14). Hit oranını asıl belirleyen şey: **key'den `state` çıkarmak** — 0-hit'in kök nedeni buydu (§2: `state` key'deyken benzersiz isteklerde hit ~0). Dal 5/7 normalizasyon/singleflight yamaları headroom `semantic_cache.py`'e yazılabilir — ayrı, opsiyonel iş; router karar katmanı etkilenmez.
- **Cache hit nereye getirir:** SO'ya giden **karar çağrısını atlar** (1500ms + 1 uçuş tasarruf) ve sticky pin ile aynı provider'a giden trafiği hızlandırır. Kazanç = karar gecikmesi + SO yükü; **cevabın cache'lenmesi iddia edilmez** (sadece tipli label, prose her hit'te yeniden üretilir). "hit ~100%" iddiası hâlâ kullanılmaz — önce 3 gün shadow'da gerçek hit adayı ölçülür.

## 3. Dal bulguları — build / skip (audit düzeltilmiş)

**Build (Go `routerd` içinde, tek binary):**
- Araç seçimi + parametre doğrulama (dal 8) — allowlist/scope önce, deterministik; JSON Schema ~20 satır; çok aday + çözülmedi → System One `choice`. **System One asla izin/scope kararı vermez.** `state`'e tool çıktısı/ham kullanıcı metni girmez. `conf>=0.8 && argmax`, retry 1 (6sn), sonra deny/ask. Fallback `openrouter .../v1/systemone`, model `typesafe/jev-1.14`. Bilinmeyen capability → yorumlanmadan bayt-for-bayt geçir; sadece security/scope kararları deterministik reddedilir (sessiz yutma YOK anlamı: security kararı sessiz geçmez). Gerekçe: SEP-2577 roots/sampling/logging deprecated (go-sdk README onaylı); 3am on-call: "desteklemiyorum" demek client'ı bozar.
- Cache politikası: **YOK** — karar cache'i eklenmez (bkz. kontrol listesi madde 13); 3. günde gerçek uniqueness ölçümü yapılmadan eklenmez.
- Gözlemlenebilirlik (dal 15): stderr'a 1 JSON satırı, allowlist `tool/route/miss_reason/decision/model + ts/dur_ms + key_hash(12 hex) + listen_addr + seq/pid + reason_code/covered/abstained/passthrough`. `conf`/`tokens`/`cost` doğrulanamadı (kendi sürecinin iddiası, shadow'da oracle yok) — allowlist dışı. **Yasak:** args, prompt, context, header, env, ham key. `jq -s` raporu. `miss_reason`, `gate_called` oranı raporlanır.
- Sağlık/fallback (dal 11): `classify(status,exc)` transient `408/429/500/502/503/504`+timeout, permanent `400/401/403/404/422` — passthrough reason-code için KALIR. Tek bool `so_degraded`: bir kez timeout/HTTP≠200 → passthrough, tekrar deneme yok. Breaker / state makinesi / `health.json` SİL (dosya I/O YAGNI). Backup aynı :20128 ise failover anlamsız (correlated failure).

**Skip (ilk sürümde YOK):**
- HITL yeni kodu (dal 9) — `walsy-reflex v0.4.0` zaten %80 yapıyor; sadece 4 hard kategori `policy.yaml`'a eklenir. Geri dönülemez işlerde HARD GATE geçerli.
- Oturum belleği (dal 12), rate limit/kota (dal 10), multi-provider ortak `generate()` arayüzü (dal 2) — mevcut altyapı varsa onlar üzerinden; yeni dosya yok. "Jev" kanıtı yok, varsayma.
- Ön-filtre (dal 6)System One kapısı — sadece 1000–4000 char, hata yok, metadata-only; `conf<0.8`→trim. Gerekirse Go içinde ~20 satır, ayrı `.mjs` yok.
- 9router `semanticCacheEntries` üzerine katman 0 (dal 5 — ölü şema), 9router patch müdahalesi.

**Endpoint (netleşti):** `POST https://evren-llmapi.ssyz.org.tr/v1/systemone`, header `x-api-key` env'den, `model:"clef"`. `localhost:20128/api/v1/systemone` + `model:"systemone"` varyantı (dal 13) kullanılmaz — 9router yamaları update'te siliniyor.

## 4. System One ortak şartnamesi

```
{ "model": "clef", "state": "...", "questions": { "<q>": { "type": "choice|score", ... } } }
```

- `questions` obje (array değil). Sadece `choice` ve `score`.
- `score` → 0..1 float + `legend` + index-keyed `probabilities`; label `legend`/argmax'dan.
- `confidence < 0.8` = geçersiz verdict: reversible → 1 kez sıkı state ile retry, irreversible → dur + insan onayı.
- Timeout / HTTP≠200 / parse hatası → fail-safe default (her dalda tanımlı).
- `state` her şeyi içerir (modelin hafızası yok), ama secret/PII/uzun context **asla**; untrusted içerik ayrı alanda.

## 5. Dosya haritası (Go — final)

```
walsy-system1/
  go.mod                      # module global-mcp-relay, stdlib only, CGO_ENABLED=0
  main.go                     # tek binary: transport seç (stdio|http|unix), env config
  mcp_relay.go                # JSON-RPC 2.0 çekirdek: id/protocolVersion/progressToken kopya (§0/§1d)
  transport_stdio.go          # type:local child — ÖNCE yazılır
  transport_unix.go           # unix socket/FIFO — 3. gün
  transport_http.go           # Streamable HTTP, MCP-Protocol-Version — 3. gün (SSE YOK)
  route.go                    # Katman 0/1/2 + fail-safe (opt-in)
  gate.go                     # System One choice/score, timeout 1500ms, retry yok
  guard.go                    # classify + tek bool so_degraded (passthrough)
  metric.go                   # stderr allowlist JSON + kapsam oranı + ret sayısı
  provider.go                 # compile-time 9 slice + providers.txt(#10) + sticky pin/rotation (§1e)
  route_test.go               # assert-based self-check
  mcp_relay_test.go           # transport-agnostik frame round-trip + extension marker (§1b)
  provider_test.go            # 9 baked / +1 satır / restart sonrası pin yok→config sırası / symlink reddi / rotation (§1e)
```

**Kaldırıldı (global pivot, m00377):** `plugin.ts`, `package.json`, `config.json`, `.env`, `com.kerem.routerd.plist` — 9router/opencode'a özel dağıtım katmanı yok. Config = `os.Getenv("SYSTEMONE_ROUTE_URL")`, `os.Getenv("NINE_ROUTER_LOG")`. Dağıtım = `go install` / brew / tek dosya indir; servis yönetimi client'ın kendi mekanizması (launchd/systemd isteğe bağlı). Ayrıca yok: `router.py`, `guard.py`, `memory.py`, `proxy.py/.mjs`, `prefilter.mjs`, `approval.mjs`, Rust `reflexd` entegrasyonu, `semanticCacheEntries` katmanı.

## 6. Uygulama sırası (kod yazmadan önce ölç)

1. **Shadow mod (3 gün):** tek binary'yi bir MCP client'ın `type:local` komutu olarak bağla, 3 gün sadece karar logla + şeffaf geçiş. 503/fail-closed açma. stderr append-only JSON line `{seq,pid,ts,decision,reason_code,covered,abstained,passthrough,latency_ms}` — `seq` boşluğu = drop/restart ayırımı için `pid` (ayrı makbuz dosyası YOK). Dağılım + gecikme + `gate_called` oranını gör.
2. **Benzersizlik ölçümü — 3. güne ertelendi:** son 1000 isteğin `sha256(state)` oranı. Bu ölçüm yapılmadan cache eklenmez (madde 13).
3. **Taşıma sırası:** **v1 sadece `transport_stdio.go`** + round-trip testi. 3. günde `transport_unix.go` + `transport_http.go` (Streamable HTTP, MCP-Protocol-Version tek yerde); SSE YOK. `argv[0]` çok-kimlik sonraya (YAGNI). HTTP modunda sabit port YOK: `127.0.0.1:0` + OS ephemeral seçimi, `listen_addr` stderr metric satırına yazılır (§1e). Port++ retry / SO_REUSEPORT / ayrı discovery dosyası YOK (madde 12, 3. gün).
4. **MCP relay testi:** extension'sız client `initialize`+tool call tamamlar; extension'lı client `com.kerem/systemone-route` marker'ını görür (§1b); `id`/`progressToken`/`protocolVersion` korunur.
5. **Provider testi:** `provider_test.go` — 9 baked / tek satır #10 / restart sonrası pin yok→config sırası / symlink reddi / deterministik rotation (§1e).
6. **Go test:** `CGO_ENABLED=0 go build ./... && go test ./...` (route/gate/guard/mcp_relay/provider self-check'leri; env set/unset assert). Guard state testi YOK (state makinesi silindi).

Kesilen: 9router/opencode'a özel TS shim + daemon plisti + config.json/.env, self-spawn, ağ kapısı, LD_PRELOAD/eBPF, yeni MCP sunucusu, provider/cache katmanı fork'u, HITL/memory/rate-limit yeni kodu, embedding/vektör DB, tüm `.py`/Rust yolları. Risk: model-öncesi garanti imkansız (spec'te `before_model` yok) — ürün "görünür kapsam ajanı" olarak konumlanmalı; SDK default-off → ilk gün kapsam oranı ~0 olabilir, ret sayısı raporlanmazsa yanıltıcı; env config restart bağımlılığı doğurur.

## 7. lrecon — PARK (m00181)

16 ADHD dalı + 3 zayıf System One turu (hepsi <0.8) sonucu: **yeni Go aracı yok, ayrı binary yok, şimdilik extend yok.** Mevcut `/Users/keremurhan/walsyrea/env/lrecon/` (635 `.go`, ~138k LOC, 235 `--op` + 80 `--ext-op`) olduğu gibi kullanılır. m00196 ile sadece *analoji* ("daima çalışan"), kapsam dışı. Notlar: docs pin bayat (`op-table.md:53` `238a7be6…` vs shipped `60581ed8…`), `native.go:1564` shell-quote eksik, `op_extract_firmware.go:50` `checkMember` doğrulanmadı — hepsi ayrı iş, bu planda yok.

## 8. Hayatta kalma matrisi — "kurulu ama bozuk" (m00559, SO `complete` p=0.627)

Tek invariant (madde 6): SO katmanının durumu ne olursa olsun client↔server trafiği birebir geçer. **503/fail-closed YOK, ilk istek gecikmez.** Aşağıdaki satırlar madde 6'nın örnekleri — ayrı kod YOK.

| Durum | Sonuç | Ek davranış |
|---|---|---|
| cold start / hiç istek yok | SO çağrısı yok, proxy kurulmamış gibi | de-energized başlangıç |
| warm-up clef 1500ms aşımı | passthrough | geç yanıt discard, retry yok |
| SO timeout / provider 500 | passthrough | sabit `so_unreachable`; URL/header/key log'a YOK |
| HTTP200 + yarım gövde (brown-out) | "karar yok" → passthrough | başarı sayılmaz |
| conf 0.799 / 0.801 | sırasıyla passthrough / pin | eşik deterministik |
| sticky pin ölümü | pin → passthrough | ayrı reason-code, self-restart yok |
| key env boş | passthrough | `degraded` ≠ `opt-out`, ayrı log |
| hot reset | passthrough | epoch artar, eski verdict sızmaz; istek başına röle, global lock yok |
| meşgul istek | passthrough | tek istek tüm akışı kilitlemez |
| 1499ms'te geç yanıt | discard | sert bütçe üstün gelir |

Kanıt katmanı madde 27'de: **tek şema** — v1 canlı = stderr tek reason-code satırı; 3 gün shadow = stderr append-only JSON line `{seq,pid,ts,decision,reason_code,covered,abstained,passthrough,latency_ms}` (ayrı makbuz DOSYASI YOK; `seq` boşluğu = drop/restart ayırımı için `pid`) + negatif kontrol. Madde 19'a ek: passthrough oranı + tipli reason-code birinci sınıf sinyal, baseline sapma alarmı. `conf`/`tokens`/`cost` doğrulanamadı (kendi sürecinin iddiası, shadow'da oracle yok) — metrik allowlist dışı. Madde 9'a ek: açılışta sadece loopback Host kontrolü (canonical fingerprint / per-process token SİL). Kapanış: bekleyen istek/state artığı bırakmaz, retry storm yok.

**3. güne ertelenir — şimdi kod YOK (YAGNI):** 0.81 zehirine karşı histerezis + dağılım, liveness nabzı (donmuş vs meşgul, tanı-only), makbuzun epoch'lar arası taşınması. Gerekirse o zaman eklenir.

**Dürüst sınır:** "bağlanmama" olayı proxy için gözlemlenemeyebilir. Vaat = "bağlanıldığında şeffafım" + kapsam oranı + ret sayısı.
## 9. Karar cache'i — ana amaç (m00752, SO pivot 0.980, §9)

İki ayrı mekanizma; ikisi de fail-safe passthrough (503 YOK).

**A. Relay karar cache'i (madde 13):** SO'nun tipi kararını (label + conf bucket + rubric version) canonical state key ile sakla. Hit = saf lokal okuma → 1500ms SO çağrısı atlanır. Sadece tipli label saklanır, prose her hit'te yeniden üretilir (bayat özet imkansız). Key = `hash(canonicalize(state) + rubricVersion + model + conf kuralı + registryVersion)`. Salt rubric yetmez — SO etiketi state'e bağlı, rubric-only replay farklı state'e yanlış etiket verir; salt ham state de 0 hit verir (mevcut headroom hatası). Rubric/deploy değişince eski cell deterministik miss. Negatif verdict (`no`/`other`/`conf<0.8`) `abstain` kodu olarak yazılır, asla label olarak servis edilmez. Admission: `conf>=0.8` VE probability margin ≥ eşik (clef overconfident 0.97–1.0 → tek yanlış label tüm client'lara yayılmasın).

**B. Provider prefix stabilizasyonu — LLM cache hit'inin asıl kaynağı:** Kabul edilen özet+skor, aynı isteğin SONUNA sabit pozisyonda **immutable blok** olarak eklenir (tidebreak 0019: compaction ayr istek değil, aynı isteğin sonu). `system`/`tools`/`messages` bayt-bayt aynı, tool array sabit sıralı (dinamik küçültme hit'i öldürür) → provider'ın kendi prefix cache'i devreye girer (Anthropic 5dk TTL, read 0.1× / write 1.25×; OpenAI min 1024 token). Katmanlar: L1 tam state hash (0 ağ) → L2 question+criteria şema hash (aynı rubric, farklı payload'da özet/skor reuse) → L3 embedding similarity **KAPALI**.

**Byte-stability mekanizması (m00894/SO 0.888):** determinism testinin nasıl geçileceği — `map[string]any` marshal YOK, sabit alan sıralı struct + tek kanonik encoder (`SetEscapeHTML(false)`, sort **AFTER** canonicalize, `FormatFloat('g',-1,64)` + `-0` fold + NaN/Inf passthrough). stdlib'de NFC/NFD yok → L1 raw + L2 `unicode.SimpleFold` alias. Tool array oturum başında snapshot + freeze (`defer_loading:true`/`tool_addition`, hepsi deferred → 400); Go SDK `sortedKeys` her add/remove'da yeniden hesaplandığı için append-only generation sıra. Breakpoint'ler tools→system→messages hiyerarşisinde 512/1024/2048/4096 token'a hizalı; sglang page-alignment (1 token sızıntı page kirletir) + system sonunda sabit guard token dizisi (vLLM lookahead hash). Prefix son baytlarının SHA-256 canary'si göndermeden önce drift'i yakalar.

**Load-bearing risk — byte drift:** özetin her istekte değişmesi / tool sırasının kayması / bloğun sabit pozisyondan taşması → her çağrı 1.25× WRITE, 0.1× READ hiç oluşmaz; hit artacağına net maliyet kaybı. Yerel hash sabit kalsa bile provider/SDK kendi kanonizasyonunu uygulayıp baytları yeniden serileştirebilir (UNVERIFIED) — canary bunu ancak gönderilen baytta yakalar. Uygulamadan ÖNCE determinism testi: aynı state+questions+criteria+tool-set için serileştirilen bayt dizisi iki kez SHA-256 → eşit olmalı; saparsa breakpoint'in nerede kırıldığını raporlar.

**Kanıt (madde 14–16) — İKİ ayrı sayaç (m00894/SO 0.888):** router karar cache hit (`reuse_precision` / `wrong_reuse`) ile provider KV hit (`match_prefix` prefix_len/prompt_len, `BlockStored(block_hashes,parent_block_hash)`) **asla tek sayıya birleştirilmez** — birleştirmek iddiayı yanlışlanamaz kılar. Yüksek provider + düşük karar = ayırt edici özellik eksik; tersi = prefill pahalı. Bélády tavanı replay block-hash dizisinden çevrimdışı hesaplanır; oracle miss etiketi `missed_reuse` (kaçırılabilir) vs `model_changed`/`tools_changed`/geo/TTL (zorunlu) ayrılır. Eşik sweep 0.70–0.75 → hit↑/accuracy↓ eğrisi; 384 kalibrasyon / 1152 test + revision hash (eşik teste sızmaz). Sayım kuralları: `respjson.Field.Valid` ile alan yokluğu 0'dan ayrılır, SSE delta usage **KÜMÜLATİF** (toplanmaz), `stream`/`stream_options` cache-affecting DEĞİL (stream hit bölme gereksiz), kesilen SSE usage = `unknown`. Falsifikasyon: hit↑ ama `wrong_reuse`↑ veya accuracy↓ ise iddia reddedilir. 3 gün shadow: key + hit/miss + margin yan yana, trafik etkilenmez. `conf`/`tokens`/`cost` allowlist dışı.

**SKIP (m00894/SO 0.151):** per-org namespace partition + idle warming/prefetch + kardeş cache süreci (self-spawn yasağı) — şimdi yok, gerekirse 3. gün.

## 11. Checker envanteri — 30 madde, hiç kaçmasın (SO `ekle_ve_say` 0.957)

Her yapılacak maddeye ≥1 doğrulama. `VAR` = mevcut checker, `EKLE` = mevcut 3 test dosyasına eklenecek assert (yeni dosya/modül YOK). 25–26 SKIP → checker yok.

| Madde | Yapılacak | Checker | Durum |
|---|---|---|---|
| 1 | tek statik binary | `CGO_ENABLED=0 go build ./...` | VAR |
| 2 | stdio relay çekirdek | `mcp_relay_test.go` frame round-trip | VAR |
| 3 | protokol sadakati | `mcp_relay_test.go` id/progressToken/protocolVersion kopya | VAR |
| 4 | opt-in extension | `mcp_relay_test.go` com.kerem marker'ı | VAR |
| 5 | route sırası | `route_test.go` Katman0/1/2 ayrı assert | EKLE |
| 6 | fail-safe | `route_test.go` conf 0.799→pass / 0.801→pin, timeout, HTTP≠200 | EKLE |
| 7 | kapsam oranı (garanti değil) | shadow stderr dağılım + ret sayısı raporu | VAR |
| 8 | 9 baked provider | `provider_test.go` (a) | VAR |
| 9 | providers.txt trust boundary | `provider_test.go` (d) key env gizli, (e) symlink reddi, (g) loopback Host | VAR |
| 10 | sticky pin + rotation | `provider_test.go` (c) restart pin yok, (f) deterministik rotation | VAR |
| 11 | SYSTEMONE_ROUTE_URL | `mcp_relay_test.go` env set→özel endpoint / unset→normal | EKLE |
| 12 | port fallback | `provider_test.go` (g) listen_addr loopback + port>0 | VAR |
| 13 | karar cache | determinism testi + canary + counterfactual replay (§9) | VAR |
| 14 | karar kanıtı | counterfactual replay + per-faz sayaç | VAR |
| 15 | canary + self-heal | 1/N replay → mismatch → cell flush assert | VAR |
| 16 | 3 gün shadow | benzersizlik ölçümü + key/hit/margin yan yana log | VAR |
| 17 | SO şeması | `route_test.go` questions obje (array DEĞİL) + choice/score + 1500ms/retry yok | EKLE |
| 18 | araç seçimi | `route_test.go` allowlist/scope önce + argmax assert | VAR (madde 24) |
| 19 | stderr allowlist | `route_test.go` secret/args/prompt/context/header/env/ham key YOK, allowlist alanları VAR | EKLE |
| 20 | sağlık/fallback | `route_test.go` classify() transient/permanent → reason-code | EKLE |
| 21 | uygulama sırası | `CGO_ENABLED=0 go build ./... && go test ./...` (§6 adım 6) | VAR |
| 22 | relay testi | `mcp_relay_test.go` (madde 22) | VAR |
| 23 | provider testi | `provider_test.go` (a–g) | VAR |
| 24 | self-check zorunluluğu | her non-trivial logic → assert; `go test ./...` | VAR |
| 25 | SKIP (HITL vb.) | checker yok — bilinçli | — |
| 26 | SKIP (self-spawn/ağ kapısı vb.) | checker yok — bilinçli | — |
| 27 | tek şema makbuz | shadow JSON line + negatif kontrol (kasıtlı bozuk istek→ret) | VAR |
| 28 | state trust boundary | `route_test.go` MRTR nested systemPrompt/message state'e girmez + secret-clean önce | EKLE |
| 29 | offline kalibrasyon | `route_test.go` PAV öncesi/sonrası ECE assert (`ece_cal<ece_raw`) + R_zero karşılaştırma | EKLE |
| 30 | dürüst kapsam oranı | `route_test.go` client×otorite matrisi: context-only/post-tool paydaya girmez | EKLE |

**Toplam: 28 checker** (19 VAR + 9 EKLE); 2 SKIP maddesi checker'sız. 9 EKLE dağılımı: `route_test.go` 8, `mcp_relay_test.go` 1 — yeni dosya/modül YOK, 503/fail-safe kuralları korunur.
