# AI Route

把多个大模型套餐（Kimi Code、GLM Coding Plan、OpenCode Go、百炼 Coding Plan、火山方舟 Coding Plan 等）聚合成**一个**对外入口。客户端只需要知道平台地址、平台发的 Key 和平台定义的模型名。上游套餐的地址、Key、模型映射和候补顺序都在 Web 后台里配置，改完立即生效。

- **两种协议都支持**：对外同时提供 OpenAI 兼容的 `/v1/chat/completions` 和 Anthropic 兼容的 `/v1/messages`（Claude Code 可以直接用）。上游有同协议端点就直通，没有就自动转换协议，流式和非流式都支持，包括工具调用和思考内容。
- **供应商**：内置 40 个预设，覆盖编码套餐、各家官方按量 API、聚合平台，选中后自动填好地址、协议、常用模型和需要的请求头，也支持自定义；每个供应商可以单独设置 User-Agent 策略。填写 Key 后模型列表会自动从上游拉取，也可以手动增删。每个套餐有一个前缀，它的模型统一显示为 `前缀/模型名`，用来区分不同套餐。
- **模型映射 + 调度顺序**：给模型起一个对外名字（如 `dess`），从全部 `前缀/模型名` 里点选要映射的模型并排好顺序，例如 `kimi/k3 → bailian/kimi-k3 → opencode/deepseek`。前一个不可用时自动切到下一个。同一优先级可以放多个上游（比如同一家的多个 Key），按权重分流。
- **先重试再切换**：遇到短暂错误（断连、5xx、上游过载、短时限流），先在同一个模型上按退避间隔重试，仍然失败才切换。这样网络抖动不会把会话切到别的套餐，避免提示词缓存失效和效果变差。
- **熔断冷却**：套餐额度用尽或 Key 失效时，整个套餐冷却；模型在重试后仍连续失败时，这个模型冷却。冷却期间排到调度顺序最后兜底，冷却时长按指数退避。
- **多个对外 Key**：可以给不同人、不同工具单独发 Key，单独停用，设置到期时间，限制可用模型，设置月预算和每分钟请求数 / tokens 上限。
- **请求日志与统计**：记录每次请求实际走了哪个上游、发生了几次切换、耗时、首字时间、token 用量和费用。概览页按模型、上游、套餐、Key 分别统计。
- **成本核算**：按量计费的供应商可以给每个模型配置输入、缓存命中、输出单价；OpenRouter 直接使用上游返回的实际费用。
- **自建模型**：vLLM、SGLang、Ollama 等自己部署的模型可以设置并发上限（满了溢出到下一个候补）、主动健康检查和单独的首包超时。
- **失效告警**：上游 Key 失效或欠费、整条调度链全部失败、长时间冷却时，推送到飞书、钉钉、企业微信群机器人或任意 Webhook。
- **单个二进制**：Go 编写，内置 SQLite 和 Web 控制台，没有外部依赖。

---

## 快速开始

### Docker Compose（推荐）

```bash
cp .env.example .env        # 修改 ADMIN_TOKEN
docker compose up -d --build
```

在国内构建时，可以把 `docker-compose.yml` 里的 `GOPROXY` 改成 `https://goproxy.cn,direct`。

打开 `http://服务器:8080/admin/`，用 `ADMIN_TOKEN` 登录。数据保存在 `ai-route-data` 卷里。

### 直接运行

```bash
go build -o bin/ai-route .
ADMIN_TOKEN=换成你的令牌 ./bin/ai-route              # 默认监听 :8080，数据在 ./data
./bin/ai-route -listen :9000 -data /var/lib/ai-route  # 也可以用参数
```

不设置 `ADMIN_TOKEN` 时，首次启动会生成一个 `admin-xxxx` 令牌并打印到日志里（同时保存在数据库）。

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `LISTEN` | `:8080` | 监听地址 |
| `DATA_DIR` | `./data` | SQLite 数据目录 |
| `ADMIN_TOKEN` | 自动生成 | 管理后台令牌 |
| `HTTPS_PROXY` / `HTTP_PROXY` | – | 访问上游时使用的代理 |

---

## 配置三步走

### 1. 添加供应商

在“供应商”页面点“添加供应商”，从预设里选一个（可以按分类筛选，也可以搜索），地址、兼容方案、常用模型、协议规则、需要的请求头都会自动填好。然后填 Key：填完会自动尝试从上游拉取模型列表；拉不到的话（很多 coding plan 不开放 `/models`），手动输入、回车添加即可。保存后也可以点“同步模型”增量拉取。选“自定义”就全部手动填写。

内置 40 个预设，定义在 [web/presets.js](web/presets.js)。同一家厂商的**编码套餐**和**按量 API** 是分开的两个预设，因为它们的地址和 Key 都不通用，比如 Kimi Code 和 Kimi 开放平台：

| 分类 | 预设 |
|---|---|
| 编码套餐 | Kimi Code（国内 / 海外）、智谱 GLM Coding Plan、Z.ai Coding Plan、阿里云百炼 Coding Plan、QwenCloud Coding、千问 Token Plan、火山方舟 Coding Plan / Agent Plan、BytePlus Coding Plan、MiniMax Token Plan（国内 / 国际）、OpenCode Go、阶跃 Step Plan、腾讯云 Token Plan、百度千帆 Token Plan、KAT-Coder |
| 官方 API（按量） | Kimi 开放平台（国内 / 海外）、智谱开放平台、Z.ai、阿里云百炼、火山方舟、DeepSeek、MiniMax、阶跃星辰、腾讯混元、小米 MiMo、美团 LongCat、OpenAI、Anthropic、Google Gemini、xAI |
| 聚合平台 | OpenCode Zen、OpenRouter、硅基流动、魔搭 ModelScope、Novita、AiHubMix、PackyCode |

> 预设整理自各家官方文档和 cc-switch 的预设（2026-10）。套餐经常调整，**请以厂商控制台为准**，添加后点“测试”验证。未能完全确认的预设会在表单里标出。

- **前缀**是供应商的唯一标识，模型映射里用 `前缀/模型名` 来引用。前缀**自动生成，不需要手填**：
  1. 选了预设就用预设的前缀，比如 Kimi Code 是 `kimi`、Kimi 开放平台是 `moonshot`；
  2. 自定义供应商从地址的域名提取，会跳过 `api`、`open`、`coding` 这类通用词，比如 `api.deepseek.com` 得到 `deepseek`，`coding.dashscope.aliyuncs.com` 得到 `dashscope`；
  3. 域名取不到时（比如 IP 地址），用名称里的字母和数字，再不行就用 `provider`；
  4. 重名时加数字后缀：`kimi`、`kimi-2`、`kimi-3`。

  前缀创建后固定不变。
- 两个地址**至少填一个**。客户端用哪种协议请求，就优先走同协议地址，没有就自动转换。
- **模型协议规则**：有些供应商的不同模型只在一种端点上提供（比如 OpenCode Go 的 MiniMax / Qwen 只走 `/messages`），可以按 `模型(支持 *) = openai|anthropic` 强制指定。相关预设已经预填好。
- **请求头透传**：调用方的请求头默认原样转发给上游，但不会转发：网关自己的凭证（`Authorization`、`x-api-key`）、`Cookie`、`Accept-Encoding`、逐跳头（`Connection` 等）、暴露用户身份的头（`X-Forwarded-*`、`X-Real-IP`、`Origin`、`Referer`、`Sec-*`、`CF-*`），以及跨协议时对面协议专属的头（`anthropic-*` / `openai-*`）。个别上游对多余请求头敏感时，可以在“高级设置”里关掉“透传调用方的请求头”。
- **自定义请求头**：每行写 `Header: 值`，会覆盖调用方的同名头，值留空表示删除该头。值可以是固定文本，也可以引用调用方的头或内置变量：

  | 写法 | 含义 |
  |---|---|
  | `X-Foo: abc` | 固定值 |
  | `X-Foo: {{header.X-Bar}}` | 取调用方的 `X-Bar`，**必传**：首选上游（调度顺序第一级里启用的供应商，含并列组）要求而调用方没带时，直接返回 400；作为候补时缺了就不发 |
  | `X-Foo: {{header.X-Bar?}}` | 取调用方的值，可选，没带就不发 |
  | `X-Foo: {{header.X-Bar ?? $conversation}}` | 调用方带了就用它的，没带由平台生成；也可以写 `?? "默认值"` |
  | `X-Foo: ai-route-{{$requestId}}` | 内置变量，可以和文字拼接 |

  内置变量：`$conversation`（`ses_` 开头，同一会话内不变：按“API Key + 第一条用户消息”计算）、`$uuid`（每个请求一个新的）、`$requestId`（网关的请求 ID，也在响应头 `X-Route-Request-Id` 和日志里）、`$timestamp`、`$keyName`、`$keyId`、`$model`（上游模型名）。变量名写错，或者引用 `header.Authorization` 这类凭证，保存时会报错。日志详情里能看到每次尝试实际发出的动态请求头。

  OpenCode Go 预设已经写好 `x-opencode-session: {{header.x-opencode-session ?? $conversation}}`：自研 Agent 传了自己的会话 ID 就原样透传，Claude Code 等不认识这个头的客户端由网关按会话生成。旧版本建的 OpenCode Go 供应商里，写死的 `ai-route` 会在启动时自动改成这个写法。
- 网关会透传 Anthropic 的 `anthropic-version` 和 `anthropic-beta`。
- **请求参数规则**：某些模型要求额外参数时使用，每行写 `模型(可用*) [条件] = JSON`。JSON 会深度合并进发给上游的请求体（在协议转换之后），值写 `null` 表示删除该字段。条件可选：`stream` / `nonstream` 限定流式或非流式，`openai` / `anthropic` / `embeddings` 限定上游协议，多个条件用逗号分隔。例如百炼的 Qwen3 开源模型默认开启思考，非流式调用必须关掉：

  ```
  qwen3-* [nonstream, openai] = {"enable_thinking": false}
  ```

  “阿里云百炼（按量）”预设已经预填了这一条。

#### User-Agent 策略

部分编码套餐会按 User-Agent 识别客户端，所以每个供应商可以单独设置：

| 策略 | 行为 | 适用 |
|---|---|---|
| 透传客户端（默认） | 原样转发调用方的 UA（如 Claude Code 的 `claude-cli/…`）；调用方没带 UA 时，用这里填写的值，没填则用平台标识 `ai-route/<版本号>` | 绝大多数情况，**Kimi Code 必须用这个** |
| 平台标识 | 所有请求都使用 `ai-route/<版本号>` | 自建模型、中转平台等需要识别来源的上游；限制客户端类型的套餐不能用 |
| 固定 UA | 所有请求都使用填写的 UA | 只在供应商明确要求某个固定 UA 时使用 |

已知要求：

- **Kimi Code**：只放行编码工具（实测 `claude-cli`、`claude-code`、`Kilo-Code` 可以），官方**禁止篡改 User-Agent**，违者可能暂停会员权益。
- **OpenCode Go**：要求客户端用自己的 UA（不要用 SDK 默认 UA），并带上稳定的 `x-opencode-session` 头。预设已经填好。
- 智谱、百炼、火山方舟的 Coding Plan 没有写明 UA 规则，但条款限定只能在编码工具里使用。

后台“测试”按钮发出的 UA 是 `ai-route-admin-test`，限制客户端的套餐可能返回 403。这时请用实际客户端经网关调用一次来确认。

#### 自建模型

自己部署的模型（vLLM、SGLang、Ollama、LM Studio 等）按“自定义”添加，地址填 `http://服务器:端口/v1`。在“高级设置 → 自建模型”里可以设置：

| 设置 | 作用 |
|---|---|
| 最大并发 | 同时在途的请求数上限。满了的请求直接溢出到调度顺序里的下一个候补，不算失败、不触发冷却。所有候补都失败、只剩满载的自建模型时，请求排队等空位，最多等“设置与接入”里的“排队等待”秒数（默认 30 秒），等不到返回 429 |
| 首包超时 | 流式请求等第一个事件的最长时间，超过就切到下一个候补。适合 GPU 排队时快速转走；收到首包之后仍按普通超时计 |
| 健康检查间隔 | 每隔这么多秒（最少 5 秒）`GET` 一次模型列表接口（`/v1/models`，也可以自定义地址），带上 Key 和自定义请求头，2xx 视为正常。连续 2 次失败就把整个供应商移出调度，只在其他候补都不可用时兜底；检查通过后自动恢复。移出和恢复都会触发告警 |

供应商列表会显示当前在途请求数和健康检查结果。

### 2. 建模型映射

在“模型映射”页面点“添加模型”：

- **对外模型名**：客户端请求时 `model` 字段填的值，如 `dess`、`coder`。
- **调度顺序**：在下方按套餐分组的模型里依次点选，点的先后就是调度顺序，之后可以用 ↑↓ 调整；再点一次就移除。不在列表里的模型，可以手动输入 `前缀/模型名`。
- **别名**（可选）：支持 `*` 通配符。例如给 `fast` 加别名 `claude-*haiku*`，Claude Code 的后台小模型请求就会落到 `fast` 上。精确名称的优先级高于通配别名。

#### 同级分流

在调度顺序里勾选“与上一项并列”，可以把多个上游放在同一优先级，比如同一个套餐的两个 Key（各建一个供应商）。每个上游可以设置权重，存储为 `kimi/k3*3 | kimi-2/k3`。

- **会话粘性**：按“API Key + 会话的第一条用户消息”做加权一致性哈希。同一个会话一直走同一个上游，保住提示词缓存；不同会话按权重分散到各个上游。没有用户消息的请求（向量、重排序）随机按权重分。
- **同级兜底**：同级里的某个上游失败时，先切到同级的其他上游，再往下一个优先级走。

示例：

| 对外模型 | 调度顺序 |
|---|---|
| `dess` | `kimi/k3` → `bailian/kimi-k3` → `opencode/deepseek` |
| `coder` | `kimi/kimi-for-coding` → `glm/glm-5.3` → `bailian/qwen3.7-plus` → `volc/ark-code-latest` |
| `fast` | `glm/glm-5.3-flash` → `volc/ark-code-latest` |
| `kimi` | `kimi/k3*2 \| kimi-2/k3`（并列，2:1 分流）→ `bailian/kimi-k3` |

#### 能力标签

每个对外模型可以打上能力标签，让使用方一眼看出它能做什么。标签显示在模型映射列表里（可以按标签筛选），也会作为扩展字段 `tags`、`description` 出现在 `/v1/models` 的返回里。

| 分组 | 标签 |
|---|---|
| 类型 | 文本对话 `chat`、向量 `embedding`、重排序 `rerank`、图像生成 `image`、语音 `audio` |
| 能力 | 多模态 `vision`、深度思考 `reasoning`、工具调用 `tools`、编程 `code`、结构化输出 `json`、联网搜索 `search` |
| 上下文（单选） | `ctx-32k`、`ctx-128k`、`ctx-200k`、`ctx-256k`、`ctx-1m` |
| 特点 | 高速 `fast`、经济 `cheap`、免费 `free` |

也可以输入自定义标签。点“根据映射的模型推荐”会按上游模型名推断标签，例如 `k3` 推出 1M 上下文，`*-flash` / `*highspeed` 推出高速，`*embedding*` / `bge-*` 推出向量，Claude / Gemini / GPT-5 推出多模态。推荐只保留**所有候补都具备**的能力，上下文取最小值；只有部分候补具备的能力会单独提示，因为切换到其他候补时这些能力可能缺失。推荐结果需要确认后才会保存。

标签只用于展示和筛选，不影响路由。

### 3. 发 API Key

在“API Keys”页面创建 Key，可以限制可用模型、设置到期时间。Key 由平台**自动生成**（`sk-route-` 加 48 位随机十六进制），不支持自定义。Key 泄露时，在编辑里点“重新生成”，旧 Key 立即失效。

每个 Key 还可以设置预算和限流（留空表示不限）：

| 限制 | 超出时 | 说明 |
|---|---|---|
| 月预算 | 返回 402（OpenAI 格式为 `insufficient_quota`，Anthropic 格式为 `billing_error`），每月 1 日恢复 | 按[成本核算](#成本核算)算出的费用累计，单位是统计货币；没配单价的请求不计入 |
| RPM（每分钟请求数） | 返回 429，带 `Retry-After` | 最近 60 秒滑动窗口 |
| TPM（每分钟 tokens） | 返回 429，带 `Retry-After` | 最近 60 秒内已完成请求的输入 + 输出 tokens；请求结束才知道用量，所以是用超之后拦截下一个请求 |

被拒绝的请求也会记进日志，不会发给上游。列表页显示每个 Key 本月已花的费用。限流计数保存在内存里，重启后清零；本月费用从日志重新统计。

---

## 客户端接入

| 协议 | Base URL | 端点 |
|---|---|---|
| OpenAI 兼容 | `http://服务器:8080/v1` | `POST /v1/chat/completions`、`POST /v1/embeddings`、`POST /v1/rerank`、`GET /v1/models` |
| Anthropic 兼容 | `http://服务器:8080` | `POST /v1/messages`、`POST /v1/messages/count_tokens` |

鉴权方式：`Authorization: Bearer sk-route-…` 或 `x-api-key: sk-route-…` 都可以。

“设置与接入”页面有**接入向导**：选好客户端（Claude Code、OpenCode、Cline / Roo Code / Kilo Code、Cherry Studio、OpenAI / Anthropic Python SDK、curl）、Key 和模型，就能生成可以直接复制的配置。

`/v1/rerank` 和 `/v1/embeddings` 一样原样转发（只改写 `model`），发往上游的 `OpenAI 兼容地址 + /rerank`，兼容 Jina、Cohere、硅基流动、vLLM 的请求格式。用量按响应里的 `usage.total_tokens`、`meta.tokens.input_tokens` 或 `meta.billed_units.input_tokens` 记为输入 tokens。

**Claude Code**

```bash
export ANTHROPIC_BASE_URL=http://服务器:8080
export ANTHROPIC_AUTH_TOKEN=sk-route-xxxx
export ANTHROPIC_MODEL=coder
export ANTHROPIC_DEFAULT_HAIKU_MODEL=fast
claude
```

**OpenAI SDK、opencode、Cline、Cherry Studio 等**：Base URL 填 `http://服务器:8080/v1`，API Key 填 `sk-route-…`，模型填对外模型名。

```bash
curl http://服务器:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-route-xxxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"coder","stream":true,"messages":[{"role":"user","content":"你好"}]}'
```

响应头 `X-Route-Target` 会标明这次实际走的是哪个上游。

---

## 成本核算

每次请求的费用记在日志里，概览页按模型、上游、套餐、Key 汇总。

- **单价**：在供应商的“高级设置”里填写，每行 `模型(可用*) = 输入 / 缓存命中 / 输出`，单位是**每百万 tokens**，货币选人民币或美元（海外预设默认美元）。缓存价可以省略，写成 `模型 = 输入 / 输出`，这时缓存命中的 token 按输入价计。精确模型名优先，其次是最长的通配规则。
- **OpenRouter**：每次响应都带实际扣费（`usage.cost`，美元），优先使用它，不需要配置单价。
- **未配单价**的模型不计费用，概览会提示有多少次请求“未配单价，未计入”，以免漏配。**包月套餐**（各家 Coding Plan）可以写一行 `* = 0 / 0`，表示不另外收费，这样就不会算作“未配单价”。
- **货币换算**：日志保留原始货币；概览统一换算成“设置与接入”里选的统计货币，汇率也在那里改。改汇率后，历史数据也按新汇率显示。
- **估算口径**：费用 = (输入 − 缓存命中) × 输入价 + 缓存命中 × 缓存价 + 输出 × 输出价。Anthropic 的缓存写入按输入价计（官方实际是输入价的 1.25 倍）；思考 token 包含在输出里。

---

## 重试、切换与熔断

一次请求按调度顺序处理每个模型。**同一个模型上先判断要不要重试，再决定是否切换**：

| 错误 | 处理 | 理由 |
|---|---|---|
| 连接断开、重置，5xx / 408 / 529，上游返回 200 但内容是错误，流式首包报错 | 在同一模型上**重试**，默认 2 次，间隔 1s、2s 翻倍；仍失败则切换 | 典型的短暂抖动，重试成本低，也能保住提示词缓存 |
| 429，且 `Retry-After` 不超过 10 秒（或没给） | 等待后重试 | 通常只是短时限流 |
| 429 要等很久、401、402 | 不重试，直接切换，并冷却**整个套餐** | 额度用尽、Key 失效、欠费，重试没有意义 |
| 404 | 直接切换，并冷却**该模型** | 模型不存在 |
| 超时 | 直接切换 | 已经等满了超时时间，再等一遍代价太大 |
| 400 / 403 / 413 等 | 直接切换，不冷却 | 一般是请求本身的问题；403 常见于 Kimi Code 的客户端白名单校验 |

**流式响应一旦开始向客户端输出，就既不能重试也不能切换**。如果中途断开，网关会给客户端发一个错误事件。

熔断按**请求**计数：某个模型在一次请求里重试完仍然失败，记为失败 1 次；**连续** 2 次（可调）后冷却该模型。冷却时长从 60 秒开始，每次连续冷却翻倍，最长 30 分钟。冷却中的模型会**排到调度顺序最后**兜底，而且不再重试，只试一次。成功一次即清零。熔断状态保存在内存里，重启后清空。

重试次数、重试间隔、冷却阈值和冷却时长都可以在“设置与接入”页面调整。重试次数设为 0 时，遇错立即切换。

### 告警通知

在“设置与接入”页面的“告警通知”里添加 Webhook，可以添加多个，每个都能单独“发送测试”：

| 类型 | 地址 | 密钥 |
|---|---|---|
| 飞书 / Lark | 群设置 → 群机器人 → 自定义机器人 | 开启“签名校验”时填 |
| 钉钉 | 群设置 → 机器人 → 自定义 | 安全设置选“加签”时填（`SEC` 开头） |
| 企业微信 | 群聊 → 添加群机器人 | 不需要 |
| 通用 JSON | 任意地址，收到 `POST {event, subject, title, text, time}` | 不需要 |

消息都以 `[AI Route]` 开头。如果机器人的安全设置用“自定义关键词”，关键词填 `AI Route` 即可。

| 事件 | 触发条件 |
|---|---|
| 鉴权失败 / 欠费 | 上游返回 401 或 402，整个套餐被冷却 |
| 调度链全部失败 | 某个对外模型的所有候补都失败，客户端收到了错误；或者它没有任何可用上游 |
| 长时间冷却 | 套餐或模型一次冷却的时长达到阈值（默认 10 分钟），比如连续多次冷却后翻倍，或者额度用尽时上游给出很长的 `Retry-After` |

同一事件、同一对象在静默时间（默认 30 分钟）内只推送一次。告警配置会随“导出配置”一起导出。

## 协议转换说明

| 客户端 → 上游 | 处理方式 |
|---|---|
| OpenAI → OpenAI、Anthropic → Anthropic | 直通，只改写 `model` 字段，其他字段原样转发 |
| OpenAI → Anthropic | `system`/`developer` 消息 → `system`；`tool_calls`/`tool` 消息 → `tool_use`/`tool_result`；图片 → image block；`reasoning_effort` → `thinking`（见下文）；`cache_control` 原样保留（写在内容块、消息或工具上都行，写在消息上时加到该消息的最后一个块）；`response_format` 见下文；流式事件转换成 chunk，包括 `reasoning_content` |
| Anthropic → OpenAI | `tool_use`/`tool_result` → `tool_calls`/`tool` 消息；`thinking` → `reasoning_content`；服务端工具（如 `web_search`）没有对应物，会被丢弃；流式 chunk 会还原成完整的 Anthropic 事件序列 |

`response_format` 的处理：上游是 Anthropic 官方（`api.anthropic.com`）且为 `json_schema` + `strict: true` 时，转成原生结构化输出 `output_config.format`；其他情况（`json_object`、非 strict 的 schema、其他厂商的 Anthropic 兼容端点）在 `system` 末尾追加一段“只输出 JSON（并符合该 schema）”的要求。兼容端点不一定认识 `output_config`，贸然发送可能被 400 拒绝。

`reasoning_effort` 的处理按上游模型区分：

| 上游模型 | 转换结果 |
|---|---|
| Claude Opus / Sonnet 4.6 及以后、Fable、Mythos | `thinking: {type: "adaptive"}` + `output_config.effort`（`minimal` 记为 `low`）。Opus 4.7 及以后、Sonnet 5 及以后、Fable 不接受 `temperature` / `top_p`，会被去掉；Opus 5.5、Sonnet 5.5、Fable 5.1 不接受强制工具调用，`tool_choice` 的 `required` / 指定函数改为 `auto` |
| 其他模型（更早的 Claude、各家兼容端点） | `thinking: {type: "enabled", budget_tokens}`，`low` 2048、`medium` 8192、`high` 16384 |

模型按名称识别，带厂商前缀的写法（如 `anthropic/claude-sonnet-5`）也能识别。

OpenAI 流式直通时，网关会自动向上游加上 `stream_options.include_usage` 来统计用量。如果客户端自己没有请求用量，这个仅含用量的 chunk 会被过滤掉，不会转发给客户端。

---

## 其他

- **备份 / 迁移**：在“设置与接入”页面可以导出或导入全部配置（JSON，包含上游 Key，请妥善保管）。
- **数据安全**：上游 Key 以明文存放在 SQLite 里，请控制好数据目录和管理令牌的访问权限。对公网开放时，建议在前面加一层 HTTPS 反向代理（Nginx/Caddy）。用 Nginx 时，流式请求需要关闭缓冲（网关已经返回了 `X-Accel-Buffering: no`）。
- **服务条款**：部分 coding plan 限制只能在官方支持的编码工具里使用，或者禁止“自建后端 / 自动化调用”（例如百炼 Coding Plan、GLM Coding Plan、Kimi Code 都有类似条款）。通过网关转发是否合规，请自行确认各家条款。
- **管理 API**：后台的所有操作都可以通过 `/admin/api/*` 完成（`Authorization: Bearer <ADMIN_TOKEN>`），例如 `GET /admin/api/providers`、`POST /admin/api/models`、`GET /admin/api/logs`、`GET /admin/api/stats?range=24h`。

## 开发

测试覆盖：

| 文件 | 内容 |
|---|---|
| `internal/gateway/mapping_test.go` | 模型映射：精确名、别名、通配别名的优先级，停用的模型，按 Key 限制模型；调度顺序（跳过不存在或停用的前缀、重试后再切换、冷却的上游排到最后）；修改配置立即生效 |
| `internal/alert`、`internal/gateway/alerts_test.go` | 告警：飞书、钉钉的签名，企业微信、通用 JSON 的格式，机器人返回 200 但带错误码，静默去重；401/402、全部失败、无可用上游、长时间冷却四种触发 |
| `internal/convert`（规则与 Claude 新模型）、`TestBodyRulesAppliedUpstream` | 请求参数规则的合并、删除、条件匹配；新版 Claude 的 adaptive thinking、effort 档位、去掉采样参数、强制工具改 auto |
| `internal/gateway/selfhost_test.go` | 自建模型：并发满时溢出到下一个候补且不计失败、排队等待空位、不排队时返回 429、首包超时只作用于流式、健康检查连续失败移出调度并告警、恢复后加回 |
| `internal/gateway/item7_test.go` 等 | 重排序转发与用量；同级分流的权重分布、会话粘性、同级兜底；目标分组的解析、校验和前缀改名 |
| `internal/hdrtpl`、`internal/gateway/headers_test.go` | 请求头模板的解析、校验和取值；透传与不透传清单；必传头只在首选上校验（冷却中也按配置顺序）、候补上缺了不发；会话 ID 在同一会话内稳定；平台标识 UA；旧 OpenCode Go 配置的自动迁移 |
| `internal/gateway/limits_test.go` | Key 限额：RPM、TPM、月预算的拦截与错误格式，被拒请求记日志且不发上游，重启后从日志恢复本月费用，切换统计货币，滑动窗口到期恢复 |
| `internal/gateway/logs_test.go` | 请求日志：成功请求每个字段的取值（Key、请求模型与对外模型、实际上游、协议、用量、客户端 IP、耗时）；费用（单价、通配单价、缓存价、未配单价、OpenRouter 实际费用）；四种协议组合下流式的用量；各种失败（模型不存在、模型不允许、全部上游失败、流中断）；按条件筛选、分页和统计 |
| `internal/admin/e2e_test.go` | 端到端：通过管理 API 创建供应商（自动前缀、重名去重、自动拉取模型）、模型映射和 Key（拒绝自定义值、重新生成），调用对外 API，再通过管理 API 查日志和统计 |
| `internal/gateway/gateway_test.go` | 协议互转、流式、重试与熔断、User-Agent 策略 |
| `internal/convert`、`internal/store` | 请求和响应格式转换、SSE 解析；前缀生成规则、导入导出、统计分桶；单价匹配与计算、费用按统计货币换算、旧数据库自动加列 |

```bash
go test -race ./...    # 用模拟上游跑全部测试（约 90 个），不需要真实 Key
go build -o bin/ai-route .
```

目录结构：

```
main.go                 入口：参数、内嵌静态资源、HTTP 服务
internal/store          SQLite 存储、内存配置快照、请求日志与统计
internal/gateway        对外 API、路由与候补切换、熔断器、后台测试
internal/convert        OpenAI ⇄ Anthropic 请求/响应/SSE 转换
internal/admin          管理 API
web/                    控制台前端（原生 JS，无构建步骤）
```
