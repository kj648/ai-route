<div align="center">

# AI Route

**Turn your pile of LLM subscriptions and API keys into one reliable, well-managed endpoint**

Kimi Code · GLM Coding Plan · Alibaba Bailian · Volcengine Ark · OpenCode Go · OpenRouter · OpenAI · Anthropic · your own vLLM … connect once, fail over automatically

[![CI](https://github.com/kj648/ai-route/actions/workflows/ci.yml/badge.svg)](https://github.com/kj648/ai-route/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/tag/kj648/ai-route?label=release)](https://github.com/kj648/ai-route/tags)
[![Go](https://img.shields.io/github/go-mod/go-version/kj648/ai-route)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[简体中文](README.md) | English

</div>

![Console overview](docs/images/en/dashboard.png)

AI Route is a self-hosted LLM API gateway. You add the endpoints and keys of your providers, give a model a public name, put upstreams in the order you want them tried, and hand out gateway keys to yourself and your team. Claude Code, Codex, Cline-style IDE extensions, Cherry Studio and any OpenAI or Anthropic SDK only talk to this one address; the gateway picks the upstream, converts the protocol, retries and fails over, enforces limits, tracks cost and alerts you when something breaks.

It is a single Go binary with an embedded SQLite database and web console, no external dependencies. `docker compose up -d` and you are running. The console speaks English and Chinese.

## Why

If you use several coding plans or pay-as-you-go APIs at once, you have probably hit these:

- **Every client is configured separately**: Claude Code one way, Codex another, Cherry Studio a third; switching a provider means editing all of them.
- **Quota runs out or a key dies mid-task**: you get a 429 halfway through and have to stop and reconfigure.
- **Protocols don't match**: Claude Code only speaks the Anthropic protocol, Codex only the OpenAI Responses API, while your providers may offer only an OpenAI endpoint or only an Anthropic one.
- **Shared use is a black box**: who used how much, what it cost, and who may use which model is unclear.

AI Route was written to solve exactly that.

## Features

**One endpoint, three protocols**
- OpenAI `/v1/chat/completions` and `/v1/responses`, Anthropic `/v1/messages`, plus `/v1/embeddings`, `/v1/rerank` and `/v1/models`.
- Same-protocol upstreams are passed through; otherwise requests are converted on the fly, streaming and non-streaming, including tool calls, reasoning, images and prompt-cache markers.
- 40 built-in provider presets (coding plans, official APIs, aggregators) that fill in endpoints, protocol rules and required headers.

**Smart routing**
- Give a model a public name (e.g. `coder`) and map it to an ordered list of upstreams; when one is unavailable the next one takes over.
- Retry before failing over: network blips, 5xx and short rate limits are retried on the same upstream first, so a conversation is not moved elsewhere and its prompt cache survives.
- Circuit breaker: when quota runs out or a key is invalid, the whole provider cools down and is only used as a last resort; cooldowns back off exponentially.
- Weighted groups: put several upstreams (e.g. two keys of the same plan) on the same priority with weights; one conversation always sticks to the same member.
- Plan quotas: give a coding plan its allowance (requests or tokens per 5 hours, day, week or month); once it is used up the plan moves behind the pay-as-you-go APIs, and comes back when the window has room.

**Management and cost**
- Multiple client keys with allowed models, expiry, monthly budget, requests per minute and tokens per minute.
- Request logs show which upstream actually answered, how many fallbacks happened, latency, time to first token, usage and cost.
- Cost tracking: set unit prices for pay-as-you-go models, OpenRouter's actual charge is used directly; the overview breaks cost down by model, upstream, provider and key.
- Alerts to Feishu/Lark, DingTalk, WeCom or any webhook, in English or Chinese.
- Request capture: to debug a client or a protocol conversion, keep the full bodies of the next few requests of a key or model; they are deleted after 24 hours.
- Secret encryption: with `SECRET_KEY` set, upstream keys, alert webhooks and captured bodies are encrypted in the database and in exports.

**Self-hosting and extensibility**
- Self-hosted models (vLLM, SGLang, Ollama …): concurrency cap with overflow, active health checks and a separate first-token timeout.
- Storage: SQLite built in, so a single binary is all you need; PostgreSQL is supported too, with a one-command data migration.
- Multiple instances: with PostgreSQL the gateway scales out; rate limits, concurrency, breaker cooldowns and configuration are shared exactly.
- Header templates: forward the caller's headers or generate values such as a per-conversation session id.
- Request body rules: inject or remove parameters per model, e.g. turn off thinking for Bailian Qwen3 non-stream calls.

## How it works

```mermaid
flowchart LR
    subgraph C[Clients]
        CC[Claude Code<br/>Anthropic protocol]
        CX[Codex CLI<br/>Responses API]
        OT[Cherry Studio / SDKs / IDE extensions<br/>OpenAI protocol]
    end
    subgraph G[AI Route]
        AUTH[Auth · rate limits · budget]
        MAP[Model mapping<br/>coder → routing order]
        CONV[Protocol conversion]
        RETRY[Retry · fail over · circuit breaker]
    end
    subgraph U[Upstreams]
        K[Kimi Code]
        GL[GLM Coding Plan]
        O[OpenCode Go]
        V[Self-hosted vLLM]
    end
    CC & CX & OT --> AUTH --> MAP --> CONV --> RETRY
    RETRY -->|primary| K
    RETRY -.->|on failure| GL
    RETRY -.-> O
    RETRY -.-> V
```

For every request: check the gateway key and its limits → find the mapping for the requested `model` → pick upstreams in routing order (cooling ones last) → convert to the upstream's protocol → send it, retrying transient errors before moving to the next upstream → convert the response back to the client's protocol → record the log and cost.

## Quick start

### Docker Compose (recommended)

```bash
git clone https://github.com/kj648/ai-route.git
cd ai-route
cp .env.example .env        # set ADMIN_TOKEN to a long random string
docker compose up -d --build
```

Open `http://your-server:8080/admin/` and log in with `ADMIN_TOKEN`. Data lives in the Docker volume `ai-route-data` and survives container rebuilds. The console follows your browser language; switch between English and 中文 at the bottom of the sidebar.

> Building in mainland China: if Docker Hub, `proxy.golang.org` or the Alpine mirrors are slow or unreachable, copy the commented block at the end of `.env.example` into `.env` (goproxy.cn, the DaoCloud image mirror and the Aliyun Alpine mirror).

### Run the binary

Requires Go 1.27 or newer:

```bash
go build -o bin/ai-route .
ADMIN_TOKEN=your-token ./bin/ai-route                 # listens on :8080, data in ./data
./bin/ai-route -listen :9000 -data /var/lib/ai-route  # flags work too
```

Without `ADMIN_TOKEN`, the first start generates an `admin-xxxx` token, prints it to the log and stores it in the database.

| Variable | Default | Meaning |
|---|---|---|
| `LISTEN` | `:8080` | Listen address |
| `DATA_DIR` | `./data` | SQLite data directory |
| `DATABASE_URL` | – | PostgreSQL URL; when set, SQLite is not used. See [Database](#database-sqlite-or-postgresql) |
| `ADMIN_TOKEN` | generated | Admin console token |
| `SECRET_KEY` | – | Encrypts upstream keys, alert webhooks and captured requests in the database; at least 16 characters. See [Secret encryption](#secret-encryption) |
| `SECRET_KEY_PREVIOUS` | – | The old `SECRET_KEY` while rotating; values are re-encrypted with the new key on start |
| `HTTPS_PROXY` / `HTTP_PROXY` | – | Proxy used to reach upstreams |
| `TRUSTED_PROXIES` | – | Reverse proxies / load balancers to trust (comma-separated IPs or CIDRs, `private` for all private ranges); requests from them are attributed to the client in `X-Forwarded-For`, for the request log and the login lockout |
| `LOG_FORMAT` | `text` | Log format; `json` for log collectors |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

### Five minutes to first request

1. **Add a provider**: on *Providers*, click *Add provider*, pick a preset (e.g. OpenRouter or Kimi Code) and enter the key. The model list is fetched from the upstream; if that is not supported, type the model names.
2. **Create a model mapping**: on *Model mappings*, add a public model such as `coder` and click the upstream models to use; the click order is the routing order.
3. **Issue a key**: on *API keys*, create a key and copy it.
4. **Connect a client**: the *Setup wizard* under *Settings* generates the configuration for your client, key and model.

Check it with curl:

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-route-your-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"coder","messages":[{"role":"user","content":"Hello"}]}'
```

The response header `X-Route-Target` tells you which upstream answered.

## Screenshots

| Providers | Model mapping: routing order and weighted groups |
|---|---|
| ![Providers](docs/images/en/providers.png) | ![Model mapping](docs/images/en/model-editor.png) |
| **Request logs** | **Request detail: every attempt and fallback** |
| ![Request logs](docs/images/en/logs.png) | ![Request detail](docs/images/en/log-detail.png) |
| **API keys: budgets and rate limits** | **Setup wizard** |
| ![API keys](docs/images/en/keys.png) | ![Setup wizard](docs/images/en/setup-wizard.png) |

## Connecting clients

| Protocol | Base URL | Endpoints |
|---|---|---|
| OpenAI compatible | `http://your-server:8080/v1` | `POST /v1/chat/completions`, `POST /v1/responses`, `POST /v1/embeddings`, `POST /v1/rerank`, `GET /v1/models` |
| Anthropic compatible | `http://your-server:8080` | `POST /v1/messages`, `POST /v1/messages/count_tokens` |

Authenticate with `Authorization: Bearer sk-route-…` or `x-api-key: sk-route-…`. The console's *Setup wizard* generates all of the following.

<details open>
<summary><b>Claude Code</b></summary>

```bash
export ANTHROPIC_BASE_URL=http://your-server:8080
export ANTHROPIC_AUTH_TOKEN=sk-route-xxxx
export ANTHROPIC_MODEL=coder
export ANTHROPIC_DEFAULT_HAIKU_MODEL=fast
claude
```

For permanent use put these into the `env` field of `~/.claude/settings.json`. Give `fast` the alias `claude-*haiku*` and Claude Code's background requests land on it automatically.
</details>

<details>
<summary><b>Codex CLI</b></summary>

Codex only speaks the Responses API; the gateway converts it for whatever upstream you map. In `~/.codex/config.toml`:

```toml
model = "coder"
model_provider = "ai-route"

[model_providers.ai-route]
name = "AI Route"
base_url = "http://your-server:8080/v1"
env_key = "AI_ROUTE_API_KEY"
wire_api = "responses"
```

Then `export AI_ROUTE_API_KEY=sk-route-xxxx` and run `codex`.
</details>

<details>
<summary><b>OpenCode</b></summary>

Add a provider to `opencode.json` in your project (or `~/.config/opencode/opencode.json`):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "ai-route": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "AI Route",
      "options": { "baseURL": "http://your-server:8080/v1", "apiKey": "sk-route-xxxx" },
      "models": { "coder": { "name": "coder" } }
    }
  }
}
```
</details>

<details>
<summary><b>Cline / Roo Code / Kilo Code, Cherry Studio, SDKs</b></summary>

- **Cline / Roo Code / Kilo Code**: API provider *OpenAI Compatible*, base URL `http://your-server:8080/v1`, model ID = the public model name.
- **Cherry Studio**: Settings → Model providers → Add, type OpenAI, API host `http://your-server:8080`.
- **OpenAI SDK**: `OpenAI(base_url="http://your-server:8080/v1", api_key="sk-route-xxxx")`
- **Anthropic SDK**: `Anthropic(base_url="http://your-server:8080", api_key="sk-route-xxxx")`
</details>

## Configuration

### Providers

Added on the *Providers* page. A vendor's **coding plan** and its **pay-as-you-go API** are separate presets because their endpoints and keys are not interchangeable (e.g. Kimi Code vs. the Moonshot platform).

| Category | Presets |
|---|---|
| Coding plans | Kimi Code (CN / global), Zhipu GLM Coding Plan, Z.ai Coding Plan, Alibaba Bailian Coding Plan, QwenCloud Coding, Qwen Token Plan, Volcengine Ark Coding Plan / Agent Plan, BytePlus Coding Plan, MiniMax Token Plan (CN / global), OpenCode Go, StepFun Step Plan, Tencent Cloud Token Plan, Baidu Qianfan Token Plan, KAT-Coder |
| Official APIs (pay-as-you-go) | Moonshot (CN / global), Zhipu, Z.ai, Alibaba Bailian, Volcengine Ark, DeepSeek, MiniMax, StepFun, Tencent Hunyuan, Xiaomi MiMo, Meituan LongCat, OpenAI, Anthropic, Google Gemini, xAI |
| Aggregators | OpenCode Zen, OpenRouter, SiliconFlow, ModelScope, Novita, AiHubMix, PackyCode |

> Presets live in [web/presets.js](web/presets.js) and were compiled from the vendors' documentation (2026-10). Plans change often, **the vendor console is authoritative**; click *Test* after adding one.

- **Prefix**: each provider has a prefix, and its models are referenced as `prefix/model`. Prefixes are generated: the preset's prefix, or one derived from the host (`api.deepseek.com` → `deepseek`), with a numeric suffix on collisions (`kimi`, `kimi-2`). They never change after creation.
- **Endpoints**: fill in the OpenAI-compatible endpoint, the Anthropic-compatible one, or both. A request uses the endpoint of its own protocol when available and is converted otherwise.
- **Model protocol rules**: when some models are only served on one endpoint, one line per rule: `model(* allowed) = openai|anthropic|responses`. E.g. OpenCode Go serves MiniMax only on `/messages` and GPT Luna only on `/responses`; the preset has these rules.
- **Responses API switch**: with *OpenAI endpoint also serves the Responses API* checked, Codex and other Responses clients are passed through; otherwise they are converted to Chat Completions. The OpenAI preset has it on; most OpenAI-compatible vendors (Kimi, GLM, DeepSeek …) have no `/responses`, so leave it off for them.
- **Unit prices**: see [Cost tracking](#cost-tracking).
- **Plan quotas**: see [Plan quotas](#plan-quotas).

#### Plan quotas

Coding plans usually allow so many requests per 5 hours or so many tokens per week, then make you wait for the window. In the provider's advanced settings, write one line per quota: `period = requests / tokens`, where 0 or left out means no limit:

```
5h = 600
week = 0 / 50000000
```

Periods are `5h` (the last 5 hours, rolling), `day` (today), `week` (this week, from Monday 00:00) and `month` (this month). Once any of them is used up, the provider moves behind the other targets in every routing order, and returns to its place when the window has room again. It is not disabled: it is still the last resort when every other target fails, and the log marks that attempt "quota used up, last resort".

- Counts come from the request log: only requests this provider answered count. They are recounted from the database every 30 seconds, with this instance's requests added in between; all instances share the same counts.
- It is the gateway's own estimate and may differ slightly from the upstream's, whose weekly window may not start on Monday, for example. A real 429 from the upstream still trips the circuit breaker as usual.
- Provider cards show the usage of each quota, and `/metrics` has `ai_route_provider_quota_used_ratio` for alerting.

#### User-Agent policy

Some coding plans identify clients by User-Agent, so each provider has its own policy:

| Policy | Behavior | Use for |
|---|---|---|
| Forward client UA (default) | Sends the caller's UA (e.g. Claude Code's `claude-cli/…`); falls back to the provider's value, then to `ai-route/<version>` | Almost everything; **Kimi Code requires this** |
| Gateway fingerprint | Always `ai-route/<version>` | Self-hosted models, relays that want to know the source |
| Fixed UA | Always the configured value | Only when a vendor asks for a specific UA |

Kimi Code's membership terms treat tampering with the client identifier (User-Agent) as a violation; OpenCode Go asks clients to use their own UA. The console's *Test* button sends `ai-route-admin-test`, which plans that restrict clients may reject with 403 — test through the gateway with a real client instead.

#### Headers

The caller's request headers are **forwarded by default**, except credentials (`Authorization`, `x-api-key`, `Cookie`), `Accept-Encoding`, hop-by-hop headers, identity-revealing ones (`X-Forwarded-*`, `Origin`, `Referer`, `Sec-*`, `CF-*`) and headers of the other protocol. Turn forwarding off in *Advanced* for upstreams that dislike extra headers.

*Custom headers* take one `Header: value` per line, override the caller's header of the same name, and an empty value removes it. Values may be templates:

| Value | Meaning |
|---|---|
| `X-Foo: abc` | Fixed value |
| `X-Foo: {{header.X-Bar}}` | The caller's `X-Bar`, **required**: missing on the primary upstream → 400 right away; missing on a fallback → not sent |
| `X-Foo: {{header.X-Bar?}}` | The caller's value, optional |
| `X-Foo: {{header.X-Bar ?? $conversation}}` | The caller's value, else generated by the gateway; `?? "default"` works too |
| `X-Trace: ai-route-{{$requestId}}` | Built-in variables, can be mixed with text |

Built-in variables: `$conversation` (`ses_…`, stable within a conversation), `$uuid` (new per request), `$requestId` (also in the `X-Route-Request-Id` response header), `$timestamp`, `$keyName`, `$keyId`, `$model`. Misspelled variables and references to credential headers are rejected on save.

The OpenCode Go preset, for example, uses:

```
x-opencode-session: {{header.x-opencode-session ?? header.x-claude-code-session-id ?? header.session-id ?? $conversation}}
```

i.e. the caller's own session id → Claude Code's `x-claude-code-session-id` → Codex's `session-id` → one generated per conversation. *Settings → Request headers* in the console lists all rules and the sources of each vendor requirement.

#### Request body rules

For models that need extra parameters, one line per rule: `model(* allowed) [conditions] = JSON`. The JSON is deep-merged into the upstream request body (after protocol conversion); `null` removes a field. Optional conditions: `stream` / `nonstream`, `openai` / `anthropic` / `responses` / `embeddings` / `rerank`. Bailian's Qwen3 open-weight models think by default and reject non-stream calls unless thinking is off; the *Alibaba Bailian (pay-as-you-go)* preset ships:

```
qwen3-* [nonstream, openai] = {"enable_thinking": false}
```

#### Self-hosted models

Add models you serve yourself (vLLM, SGLang, Ollama, LM Studio …) as a custom provider with `http://host:port/v1`. Under *Advanced → Self-hosted models*:

| Setting | Effect |
|---|---|
| Max concurrency | Caps in-flight requests. Excess requests overflow to the next fallback without counting as failures; when every other fallback failed and only full self-hosted targets remain, requests queue for a free slot (30 s by default) and then get 429 |
| First-token timeout | How long a stream may wait for its first event before failing over — hand over quickly while the GPU is queueing |
| Health check | Periodically `GET`s the models endpoint (or a custom URL); two failures in a row take the provider out of rotation, a passing check brings it back, both raise alerts |

### Model mappings

- **Public model name**: what clients put in `model`, e.g. `coder`, `fast`.
- **Routing order**: click upstream models from the grouped list below; reorder with ↑↓.
- **Weighted groups**: check *same priority as previous* to put several upstreams on one level with weights (stored as `kimi/k3*2 | kimi-2/k3`). Weighted consistent hashing on *API key + first user message* keeps a conversation on the same member (its prompt cache survives) while spreading conversations by weight; if a member fails, the others in its group are tried first.
- **Aliases**: `*` wildcards supported, e.g. give `fast` the alias `claude-*haiku*`. Exact names win over wildcard aliases.
- **Capability tags**: type, capabilities, context length, fast/cheap …, shown in the list and as extension fields in `/v1/models`. They can be suggested from the mapped models; only capabilities every fallback has are suggested.

| Public model | Example routing order |
|---|---|
| `coder` | `kimi/kimi-for-coding*2 \| kimi-2/kimi-for-coding` → `glm/glm-5.3` → `bailian/qwen3.7-plus` |
| `fast` | `glm/glm-5.3-flash` → `deepseek/deepseek-chat` |
| `gpt` | `opencode/gpt-5.6-luna` → `openrouter/openai/gpt-5.6-sol` |

### API keys

Keys are generated by the gateway (`sk-route-` + 48 hex characters); *Regenerate* invalidates a leaked key immediately. Each key can be limited to certain models, given an expiry, and:

| Limit | When exceeded | Notes |
|---|---|---|
| Monthly budget | 402 (`insufficient_quota` in OpenAI format, `billing_error` in Anthropic format), resets on the 1st | Accumulates the tracked cost in the reporting currency, from the hourly rollup, so log retention does not affect it |
| RPM | 429 with `Retry-After` | 60-second sliding window |
| TPM | 429 with `Retry-After` | Input + output tokens of requests finished in the last 60 seconds |
| Max concurrency | 429 with `Retry-After` | Requests in flight at the same time |

- Rejected requests are logged and never reach an upstream; `/v1/messages/count_tokens` counts toward the limits too.
- The monthly budget and TPM are checked when a request starts and requests already running finish normally, so high concurrency can overshoot them. Give budgeted keys a concurrency cap as well.
- When a client disconnects in the middle of a stream, the gateway keeps reading the upstream (up to 30 seconds) to bill the real usage; if it still gets none, it bills an estimate based on the request size and marks the log entry as estimated.

### Cost tracking

- **Unit prices**: in a provider's *Advanced* section, one line per rule: `model(* allowed) = input / cache hit / output`, per million tokens, in CNY or USD. The cache price is optional and defaults to the input price.
- **OpenRouter**: the actual charge in the response (`usage.cost`) is used.
- **Subscription plans**: `* = 0 / 0` marks them as free; requests without any price are flagged as "not counted" on the overview so you notice missing prices.
- **Currencies**: logs keep the original currency; the overview converts everything to the reporting currency chosen in *Settings*.
- **Formula**: (input − cache hits) × input price + cache hits × cache price + output × output price. Anthropic cache writes are counted at the input price (the actual price is 1.25×); reasoning tokens are part of the output.

### Alerts

Add webhooks under *Settings → Alerts*; each one has a *Send test* button:

| Type | Notes |
|---|---|
| Feishu / Lark | Signature verification supported |
| DingTalk | Signed requests supported (secret starting with `SEC`) |
| WeCom | Just the bot URL |
| Generic JSON | Receives `POST {event, subject, title, text, time}` |

Triggers: an upstream answers 401/402 (invalid key, out of credit), every upstream of a model failed, a provider or model is cooled down for longer than a threshold (10 minutes by default), a self-hosted model fails or recovers its health check. The same alert is sent at most once per silence window (30 minutes by default). Messages start with `[AI Route]` — use that as the keyword if your bot filters by keyword — and can be sent in English or Chinese.

### Request capture

When someone reports that a client fails through the gateway, the request log shows status and timings but not the bodies. Click *Capture requests* on the *Request log* page and pick an API key and / or model, how many requests (up to 50) and how long to wait. The next matching requests are saved in full:

- what the client sent;
- every request sent upstream (after protocol conversion) and the upstream's raw response, streams as raw SSE;
- what the client got back.

Captured requests are marked in the log; open one to view and copy the bodies. Nothing is captured by default, and without a capture rule there is no overhead. Captures contain the full prompts and only administrators can see them. Each body is capped at 1 MB, at most 200 captures are kept, and they are deleted after 24 hours; with `SECRET_KEY` set they are encrypted in the database.

## Routing, retries and circuit breaking

On each upstream the gateway **first decides whether to retry, then whether to fail over**:

| Error | Handling | Why |
|---|---|---|
| Connection reset, 5xx / 408 / 529, 200 with an error body, error as first stream event | Retry on the same upstream (2 times by default, 1 s then 2 s), then fail over | Typical transient blips; retrying keeps the prompt cache |
| 429 with `Retry-After` ≤ 10 s (or none) | Wait and retry; if it keeps failing it counts as an ordinary failure | Short rate limit |
| 429 with a long wait, 401, 402 | Fail over immediately and cool down the **whole provider** | Quota exhausted, invalid key, out of credit |
| 404 | Fail over and cool down **that model** | Model does not exist |
| Timeout | Fail over | The full timeout already passed |
| 400 / 403 / 413 …, and 429 / 404 saying the request is too large or the context too long | Fail over, no cooldown | A problem with that request — one client must not cool down a provider everybody shares |

- Once a stream has started sending data to the client it can no longer be retried or moved; if it breaks, the client gets an error event.
- Two failed requests in a row (after retries) cool an upstream down, starting at 60 seconds and doubling each time up to 30 minutes. Cooling upstreams go last and get a single try; one success resets the count.
- All of these are adjustable under *Settings*.

## Protocol conversion

| Client → upstream | Handling |
|---|---|
| Same protocol | Passed through; only `model` is rewritten |
| OpenAI ⇄ Anthropic | System messages, tool calls and results, images, reasoning and `cache_control` are mapped; stream events are converted one by one |
| Responses ⇄ Chat | `instructions` / `developer` → `system`, `function_call` ⇄ `tool_calls`; Codex's freeform tools (e.g. `apply_patch`) become a function with one string parameter and are turned back afterwards; OpenAI built-in tools (`web_search` …) have no equivalent and are dropped |
| Responses ⇄ Anthropic | Direct: `input_file` ⇄ `document`, tool results with images, `is_error` and reasoning summaries are kept; nothing goes through Chat |

Every protocol is parsed into one intermediate form (Anthropic's content-block model with a few extensions) and written out in the upstream's protocol, so any pair takes one parse and one emit. Only what the target protocol cannot express is lost: for a Chat upstream, images inside tool results move to the front of the next user message and documents are dropped; thinking the gateway synthesized from another protocol carries no signature and is removed for Anthropic upstreams.

- **`reasoning_effort`**: for Claude Opus / Sonnet 4.6+ and Fable it becomes `thinking: {type: "adaptive"}` plus `output_config.effort`, and parameters those models reject are removed; other models get `budget_tokens`.
- **`response_format`**: a strict JSON schema becomes native structured output on the official Anthropic API; otherwise a "reply with JSON only" instruction is appended to the system prompt.
- **Responses API limits**: the gateway is stateless, so `previous_response_id` and `conversation` are not supported when the upstream is not a Responses API (400). Codex sends the full history by default and is not affected.

## Database: SQLite or PostgreSQL

SQLite is built in and used by default — nothing to configure. Set `DATABASE_URL` to use PostgreSQL (12 or newer) instead; tables are created and upgraded on startup.

| | SQLite (default) | PostgreSQL |
|---|---|---|
| Good for | individuals and teams, zero setup | teams that already run PostgreSQL (backups, monitoring, HA), or want to query request logs with SQL / BI tools |
| Where data lives | `ai-route.db` in the data directory | the database you point it at |
| Backups | copy the data directory while stopped, or export the configuration in *Settings* | `pg_dump` and friends |

**Use an existing PostgreSQL**:

```bash
DATABASE_URL='postgres://airoute:password@db.example.com:5432/airoute?sslmode=require' ./bin/ai-route
```

With Docker Compose, put `DATABASE_URL` in `.env`. URL-encode characters such as `@`, `:` and `/` in the password. The gateway uses at most 16 database connections.

**Start PostgreSQL alongside with Compose**: set `POSTGRES_PASSWORD` in `.env` (e.g. `openssl rand -hex 16`), then:

```bash
docker compose -f docker-compose.yml -f docker-compose.postgres.yml up -d --build
```

**Move existing SQLite data to PostgreSQL**: configuration, the admin token and all request logs are copied, and key ids stay the same (monthly budgets depend on them). The target database must be empty.

```bash
# Docker Compose: stop the gateway, start PostgreSQL, migrate, start on PostgreSQL
docker compose stop ai-route
docker compose -f docker-compose.yml -f docker-compose.postgres.yml up -d postgres
docker compose -f docker-compose.yml -f docker-compose.postgres.yml run --rm ai-route migrate -from /data
docker compose -f docker-compose.yml -f docker-compose.postgres.yml up -d

# binary
./bin/ai-route migrate -from ./data -to 'postgres://airoute:password@db.example.com:5432/airoute'
```

The other direction works too (`-from` a PostgreSQL URL, `-to` an empty directory) if you want to go back to SQLite. One million log rows took about 13 seconds in a local test. The SQLite file is left untouched; delete it yourself once you are happy.

> With PostgreSQL you can also run several instances; see [Multi-instance deployment](#multi-instance-deployment).

## Multi-instance deployment

With PostgreSQL you can run several gateway instances behind a load balancer. They only need the same database — no Redis or other services.

```bash
# example: PostgreSQL + 3 gateway instances + Caddy load balancer on 8080; set ADMIN_TOKEN and POSTGRES_PASSWORD in .env
docker compose -f docker-compose.cluster.yml up -d --build
docker compose -f docker-compose.cluster.yml up -d --scale ai-route=5   # change the number of instances
```

**What the instances share**

| State | How | Notes |
|---|---|---|
| Providers, mappings, API keys, settings | written to the database; other instances reload within a second | edit in any instance's console |
| Key RPM / TPM and concurrency caps | exact: checked and counted atomically in the database | e.g. 1000 simultaneous requests over 3 instances with an RPM 300 key: exactly 300 get through |
| Provider max concurrency (self-hosted models) | exact; when full, requests overflow to the next fallback or queue as usual | queued requests also get slots freed by other instances |
| Breaker cooldowns | when one instance finds a plan out of quota or a key revoked, the others cool it down within a second; *Reset all* applies everywhere | consecutive-failure counts stay per instance |
| Monthly budgets | computed from every instance's hourly rollup, refreshed every 30 s | checked when a request starts, as with one instance |
| Alerts | each event is sent by one instance only | |
| Admin login lockout | counted over all instances | see `TRUSTED_PROXIES` below |
| Health checks | each instance probes on its own | alerts are sent once |
| Log cleanup | one instance at a time | |

**Notes**

- Every instance uses the same `ADMIN_TOKEN` and `DATABASE_URL`.
- The load balancer must not buffer responses (streaming). The Caddy config used by the example is [`deploy/Caddyfile`](deploy/Caddyfile); for Nginx see `proxy_buffering off` above.
- Set `TRUSTED_PROXIES` (e.g. `private` for private networks) so the gateway takes the real client IP from the load balancer's `X-Forwarded-For`. Without it the request log records the load balancer's address and the login lockout counts against it (the right token is never locked out, but the allowance for wrong attempts is shared by everyone).
- When an instance dies, its concurrency slots are released once its heartbeat is 20 seconds old; a clean stop releases them at once. *Settings → Instances* lists every instance and its heartbeat.
- Each instance uses at most 16 database connections; PostgreSQL's `max_connections` (100 by default) must exceed 16 × instances.
- Shared state lives in UNLOGGED PostgreSQL tables (no write-ahead log, so it does not slow requests down). If PostgreSQL crashes they start empty and the instances rebuild them within seconds; configuration and logs are not affected.
- Cost: keys without limits never touch the database for this; a key with limits adds about 0.6 ms per request. In a local test one limited key reached about 6,700 requests/s across 3 instances.

## Deployment and security

- **HTTPS**: put a reverse proxy in front when exposing it to the internet, with buffering off for streams (the gateway already sends `X-Accel-Buffering: no`):

  ```nginx
  location / {
      proxy_pass http://127.0.0.1:8080;
      proxy_http_version 1.1;
      proxy_set_header Host $host;
      proxy_set_header X-Real-IP $remote_addr;
      proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
      proxy_buffering off;
      proxy_read_timeout 600s;
  }
  ```

  With Caddy: `reverse_proxy 127.0.0.1:8080 { flush_interval -1 }`.
- **Admin token**: `ADMIN_TOKEN` must be at least 16 characters and not the example value (the gateway refuses to start otherwise); `openssl rand -hex 24` makes a good one. After ten wrong tokens from one IP within a minute, further wrong attempts are refused for a minute; the right token is never locked out, so colleagues behind a shared address cannot shut the administrator out. Behind a reverse proxy set `TRUSTED_PROXIES`, or every request counts as the proxy's IP.
- **Secrets**: without `SECRET_KEY`, upstream keys are stored in plain text in the database; setting it is recommended, see [Secret encryption](#secret-encryption). With SQLite the data directory is created `0700` and the database files `0600`; with PostgreSQL, give the gateway its own database user and enable TLS (`sslmode=require`). Protect the database and the admin token. The client IP in logs is taken from `X-Forwarded-For` only for requests from `TRUSTED_PROXIES`; otherwise the TCP peer is logged.
- **Error messages**: clients only see each upstream's status and the request id; raw upstream errors and URLs stay in the request log, so internal hosts and account details do not leak.
- **Headers**: when forwarding caller headers, credentials, browser headers and account-selecting ones such as `OpenAI-Organization` / `OpenAI-Project` are dropped. `anthropic-beta` is forwarded (Claude Code depends on it), so clients can enable upstream beta features, some of which change pricing. Health checks carry the provider's key — point them at the provider's own service.
- **Console**: served with a Content-Security-Policy, `X-Frame-Options: DENY` and related headers; it only loads its own scripts and styles.
- **Resource protection**: request bodies up to 64 MB and 2 minutes to send them; upstream stream events up to 8 MB; writes to a client that stops reading time out after 60 seconds; client-supplied log fields are length-capped, and space is reclaimed after old logs are deleted.
- **Backup and migration**: *Settings* can export and import the whole configuration (JSON, including upstream keys — keep it safe; with `SECRET_KEY` set they are exported encrypted).
- **Instances**: with SQLite run a single instance; with PostgreSQL you can run several, see [Multi-instance deployment](#multi-instance-deployment). After a restart breakers start fresh (or sync from the other instances) and the month's spend is recomputed from the logs.

### Secret encryption

Set `SECRET_KEY` (at least 16 characters, e.g. `openssl rand -hex 32`) and restart: the gateway then encrypts upstream keys, alert webhook URLs and signing secrets, and captured requests in the database with AES-256-GCM. Values stored in plain text before are encrypted on start. Without it, anyone holding the database file or a `pg_dump` holds every plan's key.

- **Keep it safe**: once the database holds encrypted values, the gateway refuses to start without the right `SECRET_KEY`. All instances of a cluster use the same value.
- **Exports**: exported upstream keys and webhooks are encrypted too, and import only where the same `SECRET_KEY` (or `SECRET_KEY_PREVIOUS`) is set.
- **Rotation**: put the old value in `SECRET_KEY_PREVIOUS` and the new one in `SECRET_KEY`, restart once, and everything is re-encrypted; then drop `SECRET_KEY_PREVIOUS`. `SECRET_KEY_PREVIOUS` alone, without `SECRET_KEY`, decrypts everything back to plain text.
- **Not encrypted**: client API keys (`sk-route-…`) and the admin token stay in plain text; they only give access to the gateway itself.

## Resource usage and sizing

Measured on a local load test (Apple Silicon, loopback, mock upstream sending 50 events per second per stream) — use it as a starting point for capacity planning:

| Scenario | Result |
|---|---|
| Gateway overhead (non-streaming, small request) | about 0.05 ms per request |
| Idle memory | about 20 MB |
| 1000 concurrent streams (small prompts) | about 155 MB resident, about 1.4 CPU cores, time to first byte p99 < 1 ms |
| 500 concurrent streams (200 KB prompt each) | about 500 MB resident (about 1 MB per stream, mostly the request body itself) |
| Request log | about 270 bytes per row, about 2.7 MB per 10,000 requests |
| 30-day overview (300,000 log rows, SQLite) | about 0.05 s from the hourly rollup (0.34 s scanning the logs), cached for 10 s; grows with the number of model × upstream × key × hour combinations, not with the log size |
| 1 million log rows: log page | SQLite about 0.18 s; PostgreSQL about 0.05 s |
| Log writes (non-streaming, about 46,000 requests/s) | every request logged on both SQLite and PostgreSQL, none dropped |

Memory grows with **requests in flight × request body size**; CPU grows with the number of streamed events. The gateway adds almost no latency — the upstream is usually the bottleneck.

| Size | CPU | Memory | Disk | Notes |
|---|---|---|---|---|
| Personal / small team (≤ 10 people) | 1 core | 512 MB | 2–5 GB | defaults are fine |
| Team (≤ 50 people) | 2 cores | 1–2 GB | 10 GB SSD | 30-day log retention |
| Department (200–500 people) | 4 cores | 2–4 GB | 20–50 GB SSD | keep logs 7–14 days |

- When the container has a memory limit, also set `GOMEMLIMIT` to about 80% of it (e.g. `GOMEMLIMIT=1600MiB`) so Go collects more aggressively before hitting the limit.
- Use the API key *Max concurrency* and the provider *Max concurrency* to cap peak memory, so a few clients cannot take everything.
- The overview and the monthly budgets read an hourly rollup (kept for 400 days), so shortening log retention does not affect them; the 1-hour overview still scans the raw logs. When one instance is not enough, use PostgreSQL and run several (see [Multi-instance deployment](#multi-instance-deployment)).
- On Linux, raise `net.core.somaxconn` if many clients connect at the same moment.

## FAQ

<details>
<summary><b>curl against localhost prints nothing</b></summary>

Most likely your shell has `http_proxy` set and the request went to the proxy. Try `--noproxy '*'`; if that helps, add `127.0.0.1,localhost` to `no_proxy` or make your proxy client connect to local addresses directly.
</details>

<details>
<summary><b>Kimi Code returns 403</b></summary>

Kimi Code only accepts coding agents. Keep the provider's User-Agent policy on *Forward client UA* and call it through the gateway with a real client such as Claude Code; the console's *Test* button is expected to be rejected.
</details>

<details>
<summary><b>The model list cannot be fetched</b></summary>

Many coding plans do not expose `/models`. Type the model names in the provider form and press Enter.
</details>

<details>
<summary><b>Forgot the admin token</b></summary>

Set the `ADMIN_TOKEN` environment variable and restart; it takes precedence.
</details>

<details>
<summary><b>Codex says previous_response_id is not supported</b></summary>

This only happens when the upstream is not a Responses API. Codex uses `store: false` and sends the full history by default; if your client relies on server-side state, map the model to an upstream that serves the Responses API.
</details>

## Admin API

Everything the console does is available under `/admin/api/*` (`Authorization: Bearer <ADMIN_TOKEN>`):

```bash
curl -H "Authorization: Bearer $ADMIN_TOKEN" http://127.0.0.1:8080/admin/api/providers
curl -H "Authorization: Bearer $ADMIN_TOKEN" "http://127.0.0.1:8080/admin/api/stats?range=24h"
```

Main endpoints: `providers`, `models`, `keys`, `logs`, `stats`, `status`, `settings`, `alerts`, `captures`, `export`, `import`.

## Monitoring

- **`GET /healthz`** returns `ok` while the database answers and 503 otherwise, for container and load-balancer health checks.
- **`GET /metrics`** serves Prometheus text-format metrics and requires the admin token (`authorization: { credentials: <ADMIN_TOKEN> }` in the scrape config). Labels are public models, providers and protocols, never request content:

| Metric | Meaning |
|---|---|
| `ai_route_requests_total{model,provider,inbound,status}` | requests |
| `ai_route_request_duration_seconds` / `ai_route_ttfb_seconds` | latency and time-to-first-byte histograms |
| `ai_route_tokens_total{kind=input\|cached\|output}`, `ai_route_cost_total{currency}` | usage and cost |
| `ai_route_upstream_attempts_total{target,result}`, `ai_route_fallbacks_total` | the result of every upstream attempt; requests that switched target |
| `ai_route_rejected_total{key,status}` | requests refused by limits, budgets or permissions |
| `ai_route_breaker_open{kind,name}`, `ai_route_provider_inflight` | cooling plans / models; in-flight requests on self-hosted models |
| `ai_route_provider_quota_used_ratio{provider,period,kind}` | share of a plan quota used in the current window; 1 = used up |
| `ai_route_log_dropped_total` | request-log entries lost to a persistently full queue; should stay 0 |

- **Logs** go to standard error; with `LOG_FORMAT=json` every line is one JSON object with fields such as `request` (the request id, also in the `X-Route-Request-Id` response header), `model`, `target`, `status` and `err`.

## Development

```bash
go test -race ./...      # all tests run against mock upstreams, no real keys needed
go build -o bin/ai-route .

# run the storage-related tests again on PostgreSQL (each test gets its own schema, dropped afterwards)
AI_ROUTE_TEST_DATABASE_URL='postgres://postgres:test@127.0.0.1:5432/airoute?sslmode=disable' \
  go test -race ./internal/store/... ./internal/gateway ./internal/admin
```

```
main.go               entry point: flags, embedded assets, HTTP server
internal/gateway      public API, routing and failover, breakers, limits, concurrency and health checks, headers
internal/convert      request / response / stream conversion between Chat, Responses and Anthropic
internal/store        SQLite / PostgreSQL storage, config snapshots, request logs and stats, data migration, multi-instance coordination
internal/admin        admin API
internal/alert        alert delivery (Feishu, DingTalk, WeCom, webhook)
internal/hdrtpl       header templates
web/                  console (plain JS, no build step; i18n.js holds the English strings)
```

Tests cover protocol conversion (including stream event order), retries and breakers, mappings and weighted groups, limits and budgets, cost, alert signatures, header templates, and self-hosted concurrency and health checks, all against mock upstreams. `scripts/check-i18n.mjs` makes sure every console string has an English translation.

## Roadmap

- [x] English README and console language switch
- [x] PostgreSQL storage and data migration
- [ ] Prebuilt binaries and Docker images
- [ ] Multiple admin accounts and an audit log
- [x] Multi-instance deployments (shared breaker and rate-limit state)

Requests are welcome in Issues.

## Contributing

Issues and pull requests are welcome. Before submitting:

- `gofmt -l .` prints nothing, and `go vet ./...` and `go test -race ./...` pass;
- new features come with tests (see the mock upstream in `internal/gateway/*_test.go`);
- new console text goes through `t()` with an English entry in `web/i18n.js` (`node scripts/check-i18n.mjs` checks it);
- new or changed provider presets link to the vendor's documentation in the PR.

## Disclaimer

- Some coding plans only allow use in officially supported coding tools, or forbid "self-built backends / proxying / automated calls" (Bailian Coding Plan, GLM Coding Plan and Kimi Code have such terms, for example). **Whether relaying through this project complies is your responsibility; read and follow each vendor's terms of service. You bear the consequences such as suspended accounts or charges.**
- This project is not affiliated with any vendor mentioned. Preset information is for reference only; the vendors' official documentation is authoritative.

## License

[MIT](LICENSE)
