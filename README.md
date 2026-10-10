<div align="center">

# AI Route

**把手上的多个大模型套餐，变成一个稳定、好管理的 API 入口**

Kimi Code · GLM Coding Plan · 百炼 · 火山方舟 · OpenCode Go · OpenRouter · 自建 vLLM……一次接入，自动切换

[![CI](https://github.com/kj648/ai-route/actions/workflows/ci.yml/badge.svg)](https://github.com/kj648/ai-route/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/tag/kj648/ai-route?label=release)](https://github.com/kj648/ai-route/tags)
[![Go](https://img.shields.io/github/go-mod/go-version/kj648/ai-route)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

简体中文 | [English](README.en.md)

</div>

![控制台概览](docs/images/dashboard.png)

AI Route 是一个自托管的大模型 API 网关。你把各家套餐的地址和 Key 配进去，给模型起一个对外的名字，排好"先用谁、不行再用谁"的顺序，然后给自己和团队发网关的 Key。Claude Code、Codex、Cursor 系插件、Cherry Studio 等客户端只需要连这一个地址，剩下的事情由网关处理：选上游、转换协议、出错重试和切换、限流记账、出问题告警。

单个 Go 二进制，内置 SQLite 和 Web 控制台，没有外部依赖，`docker compose up -d` 即可运行。控制台支持中文和英文。

## 为什么需要它

如果你同时用着几个编码套餐或按量 API，大概率遇到过这些问题：

- **每个客户端都要配一遍**：Claude Code 配一套，Codex 配一套，Cherry Studio 再配一套；换个套餐要改一圈。
- **额度用完、Key 失效要手动换**：写到一半被 429 打断，只能停下来改配置。
- **协议对不上**：Claude Code 只说 Anthropic 协议，Codex 只说 OpenAI Responses 协议，而你手上的套餐有的只给 OpenAI 接口，有的只给 Anthropic 接口。
- **团队共用时管不住**：谁用了多少、花了多少钱、能用哪些模型，都不清楚。

AI Route 就是为解决这些问题写的。

## 功能特性

**统一接入**
- 三种协议都提供：OpenAI 的 `/v1/chat/completions`、`/v1/responses`，Anthropic 的 `/v1/messages`，另有 `/v1/embeddings`、`/v1/rerank`、`/v1/models`。
- 上游有同协议端点就直通，没有就自动转换，流式和非流式都支持，包括工具调用、思考内容、图片和提示词缓存标记。
- 内置 40 个供应商预设（编码套餐、各家官方 API、聚合平台），选中后自动填好地址、协议规则和必要的请求头。

**智能调度**
- 给模型起对外名字（如 `coder`），按顺序映射到多个上游，前一个不可用就自动切下一个。
- 先重试再切换：网络抖动、5xx、短时限流先在同一个上游重试，避免会话被切走、提示词缓存失效。
- 熔断冷却：额度用尽、Key 失效时整个套餐冷却，冷却期间排到最后兜底，时长指数退避。
- 同级分流：同一优先级放多个上游（比如同一家的两个 Key），按权重分流，同一会话固定走同一个。
- 套餐配额：给编码套餐填上“每 5 小时 / 每天 / 每周 / 每月”的请求数或 tokens 上限，用满后自动排到按量 API 后面，窗口有余量再回来。

**管理与成本**
- 多个对外 Key：可以限制可用模型、设到期时间、月预算、每分钟请求数和 tokens。
- 请求日志记录实际走了哪个上游、切换了几次、耗时、首字时间、用量和费用。
- 成本核算：给按量模型配单价，OpenRouter 直接用实际扣费；概览按模型、上游、套餐、Key 汇总。
- 失效告警：推送到飞书、钉钉、企业微信群机器人或任意 Webhook，消息可选中文或英文。
- 抓取报文：排查客户端兼容或协议转换问题时，按 Key 或模型抓取接下来几条请求的完整报文，24 小时后自动删除。
- 密钥加密：设置 `SECRET_KEY` 后，上游 Key、告警 Webhook 和抓取的报文在数据库和导出文件里都是密文。

**自建与扩展**
- 自建模型（vLLM、SGLang、Ollama 等）：并发上限与溢出、主动健康检查、单独的首包超时。
- 请求头模板：透传调用方的头，或按规则生成会话 ID 等动态值。
- 请求参数规则：按模型给请求体注入或删除参数，比如百炼 Qwen3 非流式时关闭思考。
- 存储：默认内置 SQLite，单个二进制即可运行；也可以使用 PostgreSQL，并提供一条命令的数据迁移。
- 多实例：使用 PostgreSQL 时可以水平扩展，限流、并发、熔断冷却和配置在实例间精确共享。

## 工作原理

```mermaid
flowchart LR
    subgraph C[客户端]
        CC[Claude Code<br/>Anthropic 协议]
        CX[Codex CLI<br/>Responses 协议]
        OT[Cherry Studio / SDK / 插件<br/>OpenAI 协议]
    end
    subgraph G[AI Route]
        AUTH[鉴权 · 限流 · 预算]
        MAP[模型映射<br/>coder → 调度顺序]
        CONV[协议转换]
        RETRY[重试 · 切换 · 熔断]
    end
    subgraph U[上游]
        K[Kimi Code]
        G[GLM Coding Plan]
        O[OpenCode Go]
        V[自建 vLLM]
    end
    CC & CX & OT --> AUTH --> MAP --> CONV --> RETRY
    RETRY -->|首选| K
    RETRY -.->|失败时| G
    RETRY -.-> O
    RETRY -.-> V
```

一个请求进来后：校验网关 Key 和限额 → 按请求里的 `model` 找到映射 → 按调度顺序挑上游（冷却中的排到最后）→ 转换成上游的协议 → 发出请求，遇到短暂错误先重试，仍失败再切下一个 → 把响应转回客户端的协议 → 记日志和费用。

## 快速开始

### 方式一：Docker Compose（推荐）

```bash
git clone https://github.com/kj648/ai-route.git
cd ai-route
cp .env.example .env        # 把 ADMIN_TOKEN 改成一串足够长的随机字符串
docker compose up -d --build
```

打开 `http://服务器:8080/admin/`，用 `ADMIN_TOKEN` 登录。数据保存在 Docker 卷 `ai-route-data` 里，重建容器不会丢。控制台默认跟随浏览器语言，可以在侧边栏底部切换“中文 / English”。

> 国内构建时，如果 Docker Hub、`proxy.golang.org` 或 Alpine 官方源访问不畅，把 `.env.example` 末尾的国内配置复制到 `.env` 并取消注释即可：Go 模块走 `goproxy.cn`，基础镜像走 DaoCloud 镜像站，Alpine 软件源走阿里云。也可以先手动 `docker pull golang:1.27-alpine` 和 `docker pull alpine:3.22` 再构建。

### 方式二：直接运行

需要 Go 1.27 或更高版本：

```bash
go build -o bin/ai-route .
ADMIN_TOKEN=换成你的令牌 ./bin/ai-route                # 默认监听 :8080，数据在 ./data
./bin/ai-route -listen :9000 -data /var/lib/ai-route    # 也可以用参数
```

不设置 `ADMIN_TOKEN` 时，首次启动会生成一个 `admin-xxxx` 令牌，打印在日志里并保存到数据库。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `LISTEN` | `:8080` | 监听地址 |
| `DATA_DIR` | `./data` | SQLite 数据目录 |
| `DATABASE_URL` | – | PostgreSQL 连接地址，设置后不再使用 SQLite，见[数据库](#数据库sqlite-与-postgresql) |
| `ADMIN_TOKEN` | 自动生成 | 管理后台令牌 |
| `SECRET_KEY` | – | 加密数据库里的上游 Key、告警 Webhook 和抓取的报文，至少 16 个字符，见[密钥加密](#密钥加密) |
| `SECRET_KEY_PREVIOUS` | – | 更换 `SECRET_KEY` 时填旧的那个，启动时自动换成新密钥加密 |
| `HTTPS_PROXY` / `HTTP_PROXY` | – | 访问上游时使用的代理 |
| `TRUSTED_PROXIES` | – | 可信的反向代理 / 负载均衡地址（逗号分隔的 IP 或 CIDR，`private` 表示所有内网地址）；来自它们的请求按 `X-Forwarded-For` 识别客户端 IP，用于请求日志和登录锁定 |
| `LOG_FORMAT` | `text` | 日志格式，`json` 便于接入日志系统 |
| `LOG_LEVEL` | `info` | 日志级别：`debug` / `info` / `warn` / `error` |

### 五分钟上手

1. **添加供应商**：在“供应商”页面点“添加供应商”，选一个预设（比如 Kimi Code），填上 Key。模型列表会自动从上游拉取，拉不到就手动输入。
2. **建模型映射**：在“模型映射”页面添加一个对外模型，比如 `coder`，从下面点选要用的上游模型，点选的先后就是调度顺序。
3. **发 API Key**：在“API Keys”页面创建一个 Key，复制保存。
4. **接入客户端**：在“设置与接入”的接入向导里，选好客户端、Key 和模型，复制生成的配置。

用 curl 验证一下：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-route-你的Key" \
  -H "Content-Type: application/json" \
  -d '{"model":"coder","messages":[{"role":"user","content":"你好"}]}'
```

响应头 `X-Route-Target` 会告诉你这次实际走的是哪个上游。

## 截图

| 供应商 | 模型映射：调度顺序与同级分流 |
|---|---|
| ![供应商](docs/images/providers.png) | ![模型映射](docs/images/model-editor.png) |
| **请求日志** | **请求详情：每次尝试与切换** |
| ![请求日志](docs/images/logs.png) | ![请求详情](docs/images/log-detail.png) |
| **API Keys：预算与限流** | **接入向导** |
| ![API Keys](docs/images/keys.png) | ![接入向导](docs/images/setup-wizard.png) |

## 客户端接入

| 协议 | Base URL | 端点 |
|---|---|---|
| OpenAI 兼容 | `http://服务器:8080/v1` | `POST /v1/chat/completions`、`POST /v1/responses`、`POST /v1/embeddings`、`POST /v1/rerank`、`GET /v1/models` |
| Anthropic 兼容 | `http://服务器:8080` | `POST /v1/messages`、`POST /v1/messages/count_tokens` |

鉴权用 `Authorization: Bearer sk-route-…` 或 `x-api-key: sk-route-…` 都可以。控制台“设置与接入”里的**接入向导**能直接生成下面这些配置。

<details open>
<summary><b>Claude Code</b></summary>

```bash
export ANTHROPIC_BASE_URL=http://服务器:8080
export ANTHROPIC_AUTH_TOKEN=sk-route-xxxx
export ANTHROPIC_MODEL=coder
export ANTHROPIC_DEFAULT_HAIKU_MODEL=fast
claude
```

长期使用可以写进 `~/.claude/settings.json` 的 `env` 字段。给 `fast` 加别名 `claude-*haiku*`，Claude Code 的后台小模型请求就会自动落到它上面。
</details>

<details>
<summary><b>Codex CLI</b></summary>

Codex 只支持 Responses API，网关会按上游自动转换。在 `~/.codex/config.toml` 里写：

```toml
model = "coder"
model_provider = "ai-route"

[model_providers.ai-route]
name = "AI Route"
base_url = "http://服务器:8080/v1"
env_key = "AI_ROUTE_API_KEY"
wire_api = "responses"
```

然后 `export AI_ROUTE_API_KEY=sk-route-xxxx`，再运行 `codex`。
</details>

<details>
<summary><b>OpenCode</b></summary>

在项目根目录的 `opencode.json`（或 `~/.config/opencode/opencode.json`）里加一个 provider：

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "ai-route": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "AI Route",
      "options": { "baseURL": "http://服务器:8080/v1", "apiKey": "sk-route-xxxx" },
      "models": { "coder": { "name": "coder" } }
    }
  }
}
```
</details>

<details>
<summary><b>Cline / Roo Code / Kilo Code、Cherry Studio、各种 SDK</b></summary>

- **Cline / Roo Code / Kilo Code**：API Provider 选 OpenAI Compatible，Base URL 填 `http://服务器:8080/v1`，Model ID 填对外模型名。
- **Cherry Studio**：设置 → 模型服务 → 添加，类型选 OpenAI，API 地址填 `http://服务器:8080`。
- **OpenAI SDK**：`OpenAI(base_url="http://服务器:8080/v1", api_key="sk-route-xxxx")`
- **Anthropic SDK**：`Anthropic(base_url="http://服务器:8080", api_key="sk-route-xxxx")`
</details>

## 配置详解

### 供应商

在“供应商”页面添加。同一家厂商的**编码套餐**和**按量 API** 是两个预设，因为地址和 Key 不通用，比如 Kimi Code 和 Kimi 开放平台。

| 分类 | 预设 |
|---|---|
| 编码套餐 | Kimi Code（国内 / 海外）、智谱 GLM Coding Plan、Z.ai Coding Plan、阿里云百炼 Coding Plan、QwenCloud Coding、千问 Token Plan、火山方舟 Coding Plan / Agent Plan、BytePlus Coding Plan、MiniMax Token Plan（国内 / 国际）、OpenCode Go、阶跃 Step Plan、腾讯云 Token Plan、百度千帆 Token Plan、KAT-Coder |
| 官方 API（按量） | Kimi 开放平台（国内 / 海外）、智谱开放平台、Z.ai、阿里云百炼、火山方舟、DeepSeek、MiniMax、阶跃星辰、腾讯混元、小米 MiMo、美团 LongCat、OpenAI、Anthropic、Google Gemini、xAI |
| 聚合平台 | OpenCode Zen、OpenRouter、硅基流动、魔搭 ModelScope、Novita、AiHubMix、PackyCode |

> 预设定义在 [web/presets.js](web/presets.js)，整理自各家官方文档（2026-10）。套餐经常调整，**请以厂商控制台为准**，添加后点“测试”验证。

- **前缀**：每个供应商有一个前缀，它的模型在映射里写作 `前缀/模型名`。前缀自动生成：预设用预设的前缀；自定义的从域名提取（`api.deepseek.com` → `deepseek`）；重名时加数字后缀（`kimi`、`kimi-2`）。创建后固定不变。
- **地址**：OpenAI 兼容地址和 Anthropic 兼容地址至少填一个。客户端用哪种协议，就优先走同协议的地址，没有就自动转换。
- **模型协议规则**：某些模型只在一种端点上提供时使用，每行 `模型(可用*) = openai|anthropic|responses`。比如 OpenCode Go 的 MiniMax 只走 `/messages`，GPT Luna 只走 `/responses`，预设已填好。
- **Responses API 开关**：勾选“OpenAI 地址也支持 Responses API”后，Codex 等客户端的请求原样转发；不勾选就转成 Chat Completions。OpenAI 官方预设已勾选；Kimi、GLM、DeepSeek 等多数兼容厂商没有 `/responses`，不要勾。
- **单价**：见[成本核算](#成本核算)。
- **套餐配额**：见[套餐配额](#套餐配额)。

#### 套餐配额

编码套餐一般限制“每 5 小时多少次请求”或“每周多少 tokens”，用完要等窗口恢复。在供应商的“高级设置”里每行写一条：`周期 = 请求数 / tokens`，0 或省略表示不限，例如：

```
5h = 600
week = 0 / 50000000
```

周期可以是 `5h`（最近 5 小时，滚动计算）、`day`（今天）、`week`（本周，从周一 0 点算）、`month`（本月）。任何一条用满后，这个供应商在所有模型的调度顺序里排到其他候补后面，窗口有余量后自动回到原位。它不会被禁用：其他候补都失败时仍会兜底，日志里这次尝试标为“配额用完兜底”。

- 计数来自请求日志：只统计这个供应商实际应答的请求，每 30 秒从数据库重新统计一次，期间本实例的请求实时累加；多实例共享同一份计数。
- 这是网关自己的估算，和上游的统计可能略有出入，比如上游的周窗口不一定从周一开始。上游真的返回 429 时，熔断仍会照常生效。
- 供应商卡片上显示每条配额的用量，`/metrics` 里有 `ai_route_provider_quota_used_ratio`，可以配告警。

#### User-Agent 策略

部分编码套餐按 User-Agent 识别客户端，每个供应商可以单独设置：

| 策略 | 行为 | 适用 |
|---|---|---|
| 透传客户端（默认） | 转发调用方的 UA（如 Claude Code 的 `claude-cli/…`）；调用方没带时用供应商里填的值，再没有就用 `ai-route/<版本号>` | 绝大多数情况，**Kimi Code 必须用这个** |
| 平台标识 | 总是发 `ai-route/<版本号>` | 自建模型、中转平台等需要识别来源的上游 |
| 固定 UA | 总是发填写的值 | 只在厂商明确要求某个固定 UA 时使用 |

Kimi Code 的会员条款规定篡改客户端标识（User-Agent）视为违规；OpenCode Go 要求客户端用自己的 UA。后台“测试”按钮发出的 UA 是 `ai-route-admin-test`，限制客户端的套餐可能返回 403，这时请用实际客户端经网关测一次。

#### 请求头

调用方的请求头**默认透传**给上游，但凭证（`Authorization`、`x-api-key`、`Cookie`）、`Accept-Encoding`、逐跳头、暴露用户身份的头（`X-Forwarded-*`、`Origin`、`Referer`、`Sec-*`、`CF-*`）以及对面协议专属的头不会转发。个别上游对多余请求头敏感时，可以在“高级设置”里关掉透传。

“自定义请求头”每行写 `Header: 值`，会覆盖调用方的同名头，值留空表示删除。值支持模板：

| 写法 | 含义 |
|---|---|
| `X-Foo: abc` | 固定值 |
| `X-Foo: {{header.X-Bar}}` | 取调用方的 `X-Bar`，**必传**：首选上游要求而调用方没带时直接返回 400；候补上游缺了就不发 |
| `X-Foo: {{header.X-Bar?}}` | 取调用方的值，可选 |
| `X-Foo: {{header.X-Bar ?? $conversation}}` | 调用方带了用它的，没带由平台生成；也可以写 `?? "默认值"` |
| `X-Trace: ai-route-{{$requestId}}` | 内置变量，可以和文字拼接 |

内置变量：`$conversation`（`ses_` 开头，同一会话内不变）、`$uuid`（每个请求一个新的）、`$requestId`（网关请求 ID，也在响应头 `X-Route-Request-Id` 里）、`$timestamp`、`$keyName`、`$keyId`、`$model`。变量写错或者引用凭证类的头，保存时会报错。

例如 OpenCode Go 预设写的是：

```
x-opencode-session: {{header.x-opencode-session ?? header.x-claude-code-session-id ?? header.session-id ?? $conversation}}
```

依次取：调用方自己的会话 ID → Claude Code 自带的 `x-claude-code-session-id` → Codex 自带的 `session-id` → 网关按会话生成。控制台“设置与接入 → 请求头说明”整理了完整规则和各厂商要求的出处。

#### 请求参数规则

某些模型要求额外参数时使用，每行写 `模型(可用*) [条件] = JSON`，JSON 会深度合并进发给上游的请求体（协议转换之后），值写 `null` 表示删除该字段。条件可选：`stream` / `nonstream`，`openai` / `anthropic` / `responses` / `embeddings` / `rerank`。例如百炼的 Qwen3 开源模型默认开启思考，非流式调用必须关掉（“阿里云百炼（按量）”预设已内置）：

```
qwen3-* [nonstream, openai] = {"enable_thinking": false}
```

#### 自建模型

自己部署的模型（vLLM、SGLang、Ollama、LM Studio 等）按“自定义”添加，地址填 `http://服务器:端口/v1`，在“高级设置 → 自建模型”里可以设置：

| 设置 | 作用 |
|---|---|
| 最大并发 | 同时在途的请求上限。满了直接溢出到下一个候补，不算失败；所有候补都失败、只剩满载的自建模型时，排队等空位（默认最多 30 秒），等不到返回 429 |
| 首包超时 | 流式请求等第一个事件的最长时间，超过就切换，适合 GPU 排队时快速转走 |
| 健康检查 | 定期 `GET` 模型列表接口（也可以自定义地址），连续 2 次失败就移出调度，恢复后自动加回，移出和恢复都会告警 |

### 模型映射

- **对外模型名**：客户端请求里 `model` 填的值，如 `coder`、`fast`。
- **调度顺序**：从下面按套餐分组的模型里依次点选，之后可以用 ↑↓ 调整。
- **同级分流**：勾选“与上一项并列”把多个上游放在同一优先级并设置权重（存储为 `kimi/k3*2 | kimi-2/k3`）。按“API Key + 会话第一条用户消息”做加权一致性哈希：同一会话固定走同一个上游以保住提示词缓存，不同会话按权重分散；其中一个失败时先切到同级的其他上游。
- **别名**：支持 `*` 通配，比如给 `fast` 加别名 `claude-*haiku*`。精确名称优先于通配别名。
- **能力标签**：类型、能力、上下文长度、高速 / 经济等，显示在列表里，也会出现在 `/v1/models` 的扩展字段里。可以根据映射的模型一键推荐，只推荐所有候补都具备的能力。

| 对外模型 | 调度顺序示例 |
|---|---|
| `coder` | `kimi/kimi-for-coding*2 \| kimi-2/kimi-for-coding` → `glm/glm-5.3` → `bailian/qwen3.7-plus` |
| `fast` | `glm/glm-5.3-flash` → `deepseek/deepseek-chat` |
| `gpt` | `opencode/gpt-5.6-luna` → `openrouter/openai/gpt-5.6-sol` |

### API Key

Key 由平台自动生成（`sk-route-` 加 48 位十六进制），泄露时点“重新生成”，旧 Key 立即失效。每个 Key 可以限制可用模型、设置到期时间，以及：

| 限制 | 超出时 | 说明 |
|---|---|---|
| 月预算 | 返回 402（OpenAI 格式为 `insufficient_quota`，Anthropic 格式为 `billing_error`），每月 1 日恢复 | 按成本核算的费用累计，单位是统计货币；来自小时聚合表，不受日志保留天数影响 |
| RPM | 返回 429，带 `Retry-After` | 最近 60 秒滑动窗口 |
| TPM | 返回 429，带 `Retry-After` | 最近 60 秒内已完成请求的输入 + 输出 tokens |
| 并发上限 | 返回 429，带 `Retry-After` | 同时在途的请求数 |

- 被拒绝的请求也会记日志，不会发给上游；`/v1/messages/count_tokens` 同样计入限流。
- 月预算和 TPM 只在请求开始时检查，正在进行的请求会照常完成，所以并发越高越可能超出。设了预算的 Key 建议同时设并发上限。
- 流式请求中途断开时，网关会继续读完上游（最多 30 秒）拿到真实用量再记账；仍拿不到时按请求大小估算，日志里会标注“用量为估算”。

### 成本核算

- **单价**：在供应商的“高级设置”里，每行 `模型(可用*) = 输入 / 缓存命中 / 输出`，单位是每百万 tokens，货币可选人民币或美元。缓存价可以省略，省略时按输入价计。
- **OpenRouter**：直接使用响应里的实际扣费（`usage.cost`）。
- **包月套餐**：写一行 `* = 0 / 0` 表示不另外收费；没配单价的请求会在概览里提示“未计入”，以免漏配。
- **货币换算**：日志保留原始货币，概览统一换算成设置里的统计货币。
- **口径**：费用 = (输入 − 缓存命中) × 输入价 + 缓存命中 × 缓存价 + 输出 × 输出价。Anthropic 的缓存写入按输入价计（官方实际是 1.25 倍），思考 tokens 包含在输出里。

### 告警通知

在“设置与接入 → 告警通知”里添加 Webhook，每个都可以单独发送测试：

| 类型 | 说明 |
|---|---|
| 飞书 / Lark | 支持签名校验 |
| 钉钉 | 支持“加签”（`SEC` 开头的密钥） |
| 企业微信 | 群机器人地址即可 |
| 通用 JSON | 收到 `POST {event, subject, title, text, time}` |

触发条件：上游返回 401 / 402（Key 失效、欠费）、某个模型的整条调度链全部失败、套餐或模型一次冷却超过阈值（默认 10 分钟）、自建模型健康检查失败或恢复。同一件事在静默时间（默认 30 分钟）内只推送一次。消息都以 `[AI Route]` 开头，机器人用“自定义关键词”时填 `AI Route` 即可；推送语言可选中文或英文。

### 抓取报文

用户反馈“某个客户端经过网关就报错”时，请求日志只有状态和耗时，看不到报文。这时在“请求日志”页点“抓取报文”，选择 API Key 和 / 或模型、抓几条（最多 50）、最长等多久，接下来匹配的请求会完整保存：

- 客户端发来的请求；
- 每次发给上游的请求（协议转换之后）和上游的原始响应，流式响应保存原始 SSE；
- 最后返回给客户端的内容。

抓到的请求在日志里标为“报文”，点开详情就能查看和复制。默认不抓任何请求，没有抓取规则时没有额外开销。报文含完整的提示词，只有管理员可见；每段最多保存 1 MB，最多保留 200 条，24 小时后自动删除；设置了 `SECRET_KEY` 时加密落库。

## 路由、重试与熔断

同一个上游上**先判断要不要重试，再决定是否切换**：

| 错误 | 处理 | 理由 |
|---|---|---|
| 断连、5xx / 408 / 529、200 但内容是错误、流式首包报错 | 同一上游重试（默认 2 次，间隔 1s、2s），仍失败再切换 | 典型的短暂抖动，重试能保住提示词缓存 |
| 429 且 `Retry-After` ≤ 10 秒（或没给） | 等待后重试，仍失败按普通失败计数 | 短时限流 |
| 429 要等很久、401、402 | 直接切换，并冷却**整个套餐** | 额度用尽、Key 失效、欠费 |
| 404 | 直接切换，并冷却**该模型** | 模型不存在 |
| 超时 | 直接切换 | 已经等满了超时时间 |
| 400 / 403 / 413 等，以及提示“请求过大 / 上下文过长”的 429、404 | 直接切换，不冷却 | 请求本身的问题，不能让一个客户端把大家共用的套餐冷却掉 |

- 流式响应一旦开始向客户端输出，就不能再重试或切换；中途断开时，网关会给客户端发一个错误事件。
- 一个上游连续 2 次请求失败（重试后仍失败）就冷却，时长从 60 秒起每次翻倍，最长 30 分钟。冷却中的上游排到最后兜底，只试一次；成功一次即清零。
- 以上参数都可以在“设置与接入”里调整。

## 协议转换

| 客户端 → 上游 | 处理方式 |
|---|---|
| 同协议 | 直通，只改写 `model` 字段 |
| OpenAI ⇄ Anthropic | 系统消息、工具调用与结果、图片、思考内容、`cache_control` 互相转换；流式事件逐个转换 |
| Responses ⇄ Chat | `instructions` / `developer` → `system`，`function_call` ⇄ `tool_calls`；Codex 的自定义工具（如 `apply_patch`）转成单个字符串参数的函数，结果再还原；内置工具（`web_search` 等）没有对应物会被丢弃 |
| Responses ⇄ Anthropic | 直接转换：`input_file` ⇄ `document`，带图片的工具结果、`is_error`、推理摘要都保留，不经过 Chat |

三种协议都先解析成同一个中间表示（以 Anthropic 的内容块模型为基础），再写成上游的协议，所以任意两种协议之间只经过一次解析和一次生成。只有目标协议表达不了的内容才会丢失：发给 Chat 上游时，工具结果里的图片会提到下一条用户消息前面，文档会被丢弃；网关从其他协议合成的思考内容没有签名，发给 Anthropic 上游时会去掉。

- **`reasoning_effort`**：发给 Claude Opus / Sonnet 4.6 及以后、Fable 时转成 `thinking: {type: "adaptive"}` 加 `output_config.effort`，并去掉新模型不接受的参数；发给其他模型时转成 `budget_tokens`。
- **`response_format`**：上游是 Anthropic 官方且为严格 JSON Schema 时，转成原生结构化输出；其他情况在系统提示词末尾要求“只输出 JSON”。
- **Responses API 的限制**：网关不保存会话状态，所以转到非 Responses 上游时不支持 `previous_response_id` 和 `conversation`（返回 400，Codex 默认每次发完整历史，不受影响）。

## 数据库：SQLite 与 PostgreSQL

默认使用内置的 SQLite，不需要任何配置。设置了 `DATABASE_URL` 就改用 PostgreSQL（12 及以上），表结构在启动时自动创建和升级。

| | SQLite（默认） | PostgreSQL |
|---|---|---|
| 适合 | 个人、团队，开箱即用 | 已有 PostgreSQL 运维（备份、监控、高可用），或想直接用 SQL / BI 工具查请求日志 |
| 数据位置 | 数据目录里的 `ai-route.db` | 你指定的数据库 |
| 备份 | 停服后复制数据目录，或在“设置与接入”导出配置 | 用 `pg_dump` 等常规手段 |

**连接已有的 PostgreSQL**：

```bash
DATABASE_URL='postgres://airoute:密码@db.example.com:5432/airoute?sslmode=require' ./bin/ai-route
```

Docker Compose 部署时把 `DATABASE_URL` 写进 `.env` 即可。密码里有 `@`、`:`、`/` 等字符时需要做 URL 编码。网关最多同时使用 16 个数据库连接。

**用 Compose 一起启动 PostgreSQL**：在 `.env` 里设置 `POSTGRES_PASSWORD`（建议 `openssl rand -hex 16`），然后：

```bash
docker compose -f docker-compose.yml -f docker-compose.postgres.yml up -d --build
```

**把已有 SQLite 数据迁移到 PostgreSQL**：配置、管理令牌、请求日志全部复制，Key 的 ID 保持不变（月预算统计依赖它）。目标数据库必须是空的。

```bash
# Docker Compose：先停网关，启动 PostgreSQL，迁移，再用 PostgreSQL 启动
docker compose stop ai-route
docker compose -f docker-compose.yml -f docker-compose.postgres.yml up -d postgres
docker compose -f docker-compose.yml -f docker-compose.postgres.yml run --rm ai-route migrate -from /data
docker compose -f docker-compose.yml -f docker-compose.postgres.yml up -d

# 直接运行
./bin/ai-route migrate -from ./data -to 'postgres://airoute:密码@db.example.com:5432/airoute'
```

反方向（`-from` 填 PostgreSQL 地址、`-to` 填一个空目录）同样可以，用来退回 SQLite。本机实测 100 万条日志迁移约 13 秒。原来的 SQLite 文件不会被修改，确认无误后再自行删除。

> 使用 PostgreSQL 后还可以部署多个实例，见下文[多实例部署](#多实例部署)。

## 多实例部署

使用 PostgreSQL 时，可以在负载均衡后面运行多个网关实例，所有实例连接同一个数据库即可，不需要 Redis 或其他组件。

```bash
# 示例：PostgreSQL + 3 个网关实例 + Caddy 负载均衡（对外 8080），.env 里设置 ADMIN_TOKEN 和 POSTGRES_PASSWORD
docker compose -f docker-compose.cluster.yml up -d --build
docker compose -f docker-compose.cluster.yml up -d --scale ai-route=5   # 调整实例数
```

**实例之间共享什么**

| 状态 | 共享方式 | 说明 |
|---|---|---|
| 供应商、模型映射、API Key、设置 | 写入数据库，其他实例 1 秒内重新加载 | 在任意实例的控制台修改都可以 |
| Key 的 RPM / TPM、并发上限 | 精确共享：检查和计数在数据库里原子完成 | 比如 RPM 300 的 Key 在 3 个实例上同时打 1000 个请求，正好放行 300 个 |
| 供应商最大并发（自建模型） | 精确共享，满了照常溢出到下一个候补或排队 | 排队的请求也能拿到其他实例释放的名额 |
| 熔断冷却 | 一个实例发现额度用尽、Key 失效等，其他实例 1 秒内跟着冷却；“全部重置”对所有实例生效 | 连续失败计数各实例自己算 |
| 月预算 | 按全部实例的小时聚合统计，30 秒刷新一次 | 和单实例一样只在请求开始时检查 |
| 告警 | 同一事件只由一个实例发送 | |
| 登录失败锁定 | 按全部实例累计 | 见下方 `TRUSTED_PROXIES` |
| 健康检查 | 每个实例各自探测 | 告警只发一次 |
| 日志清理 | 同一时间只有一个实例执行 | |

**注意事项**

- 所有实例使用同一个 `ADMIN_TOKEN` 和 `DATABASE_URL`。
- 负载均衡要关闭响应缓冲（流式输出）。示例用的 Caddy 配置在 [`deploy/Caddyfile`](deploy/Caddyfile)，Nginx 参考上文 `proxy_buffering off`。
- 设置 `TRUSTED_PROXIES`（例如 `private`，表示内网地址），网关才会从负载均衡传来的 `X-Forwarded-For` 里取真实客户端 IP。不设置时，请求日志记录的是负载均衡的地址，登录失败锁定也按它计算（正确的令牌不受锁定影响，但错误尝试的额度是所有人共用的）。
- 实例异常退出时，它占用的并发名额在 20 秒没有心跳后自动释放；正常停止时立即释放。“设置与接入”页面的“运行实例”列出所有实例和心跳。
- 每个实例最多使用 16 个数据库连接，PostgreSQL 的 `max_connections`（默认 100）要大于 16 × 实例数。
- 共享状态放在 PostgreSQL 的 UNLOGGED 表里（不写 WAL，不会拖慢请求）。PostgreSQL 崩溃重启后这些表会被清空，几秒内由各实例重建，配置和日志不受影响。
- 开销：没有设置限额的 Key 不访问数据库；设置了限额的 Key，每个请求多约 0.6 ms。本机测试中单个限额 Key 可以达到约 6700 请求/秒（3 个实例）。

## 部署与安全

- **HTTPS**：对公网开放时，建议在前面加一层反向代理。流式响应需要关闭缓冲（网关已返回 `X-Accel-Buffering: no`）：

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

  Caddy 只需要一行：`reverse_proxy 127.0.0.1:8080 { flush_interval -1 }`。
- **管理令牌**：`ADMIN_TOKEN` 至少 16 个字符，不能用示例值（否则拒绝启动），可以用 `openssl rand -hex 24` 生成。同一 IP 1 分钟内输错 10 次，之后的错误尝试会被拒绝 1 分钟；正确的令牌不受锁定影响，所以共用出口 IP 的同事不会把管理员锁在外面。网关在反向代理后面时请设置 `TRUSTED_PROXIES`，否则所有请求都算作代理的 IP。
- **数据安全**：不设置 `SECRET_KEY` 时上游 Key 以明文存放在数据库里，建议设置，见下方[密钥加密](#密钥加密)。SQLite 的数据目录权限为 `0700`、数据库文件为 `0600`；使用 PostgreSQL 时请给网关单独的数据库账号，并开启 TLS（`sslmode=require`）。请控制好数据库和管理令牌的访问权限。日志里的客户端 IP 只在请求来自 `TRUSTED_PROXIES` 时取自 `X-Forwarded-For`，其余情况记录 TCP 对端地址。
- **错误信息**：返回给客户端的错误只包含各上游的状态码和请求 ID，上游的原始报错、地址留在请求日志里，避免泄露内部地址或账户信息。
- **请求头**：转发调用方请求头时，凭证类、浏览器类以及 `OpenAI-Organization` / `OpenAI-Project` 这类会切换账户的头都会被去掉；`anthropic-beta` 会透传（Claude Code 依赖它），客户端因此可以启用上游的 beta 功能，部分 beta 会改变计费。健康检查会带上供应商的 Key，地址请填它自己的服务。
- **控制台**：返回 CSP、`X-Frame-Options: DENY` 等安全头，只加载自身的脚本和样式。
- **资源保护**：单个请求体最多 64 MB，读取请求体最多 2 分钟；上游单个流式事件最多 8 MB；客户端不读数据时，单次写入 60 秒超时；日志里客户端提供的字段有长度上限，过期日志删除后会回收数据库空间。
- **备份与迁移**：“设置与接入”里可以导出 / 导入全部配置（JSON，包含上游 Key，请妥善保管；设置了 `SECRET_KEY` 时导出的是密文）。
- **实例数**：使用 SQLite 时只能运行一个实例；使用 PostgreSQL 时可以运行多个，见[多实例部署](#多实例部署)。重启后熔断状态清空（多实例时从其他实例同步），本月费用从日志重新统计。

### 密钥加密

设置环境变量 `SECRET_KEY`（至少 16 个字符，可以用 `openssl rand -hex 32` 生成）后重启，网关会用 AES-256-GCM 加密数据库里的上游 Key、告警 Webhook 地址与签名密钥、抓取的报文；已有的明文值在启动时自动加密。没有它，拿到数据库文件或 `pg_dump` 的人就拿到了所有套餐的 Key。

- **必须保管好**：数据库里有密文时，不设置或设错 `SECRET_KEY` 网关会拒绝启动。多实例时所有实例用同一个值。
- **导出文件**：导出的上游 Key 和 Webhook 也是密文，只能导入到 `SECRET_KEY`（或 `SECRET_KEY_PREVIOUS`）相同的实例。
- **更换密钥**：把旧值放进 `SECRET_KEY_PREVIOUS`、新值放进 `SECRET_KEY`，重启一次，全部会换成新密钥；之后可以去掉 `SECRET_KEY_PREVIOUS`。只设 `SECRET_KEY_PREVIOUS` 不设 `SECRET_KEY`，就是解密回明文。
- **不加密的部分**：对外 API Key（`sk-route-…`）和管理令牌仍是明文，它们只能访问网关本身。

## 资源占用与配置建议

以下数据来自本机压测（Apple Silicon，回环网络，模拟上游每个流每秒 50 个事件），可作为容量规划的参考：

| 场景 | 结果 |
|---|---|
| 网关额外延迟（非流式，小请求） | 约 0.05 ms / 请求 |
| 空闲内存 | 约 20 MB |
| 1000 个并发流（小 prompt） | 常驻内存约 155 MB，约 1.4 核 CPU，首字节 p99 < 1 ms |
| 500 个并发流（每个 prompt 200 KB） | 常驻内存约 500 MB（约 1 MB / 流，主要是请求体本身） |
| 请求日志 | 每条约 270 字节，1 万次请求约 2.7 MB |
| 30 天概览（30 万条日志，SQLite） | 从小时聚合表读取约 0.05 秒（直接扫日志约 0.34 秒），结果缓存 10 秒；耗时随“模型 × 上游 × Key × 小时”的组合数增长，与日志条数无关 |
| 100 万条日志：日志翻页 | SQLite 约 0.18 秒；PostgreSQL 约 0.05 秒 |
| 日志写入（非流式，约 4.6 万请求/秒） | SQLite 和 PostgreSQL 都能完整记录，不丢日志 |

内存主要随**同时进行中的请求数 × 请求体大小**增长，CPU 主要随流式事件数增长；网关本身几乎不增加延迟，瓶颈通常在上游。

| 规模 | CPU | 内存 | 磁盘 | 说明 |
|---|---|---|---|---|
| 个人 / 小团队（≤ 10 人） | 1 核 | 512 MB | 2–5 GB | 默认配置即可 |
| 团队（≤ 50 人） | 2 核 | 1–2 GB | 10 GB SSD | 日志保留 30 天 |
| 部门（200–500 人） | 4 核 | 2–4 GB | 20–50 GB SSD | 建议日志保留 7–14 天 |

- 给容器设置内存上限时，同时设置 `GOMEMLIMIT`（约为上限的 80%，如 `GOMEMLIMIT=1600MiB`），让 Go 在接近上限前更积极地回收内存。
- 用 API Key 的“并发上限”和供应商的“最大并发”控制峰值内存，避免少数客户端占满资源。
- 概览和月预算读取按小时预聚合的统计表（保留 400 天），缩短日志保留天数不影响它们；1 小时范围的概览仍按原始日志统计。单个实例不够用时，改用 PostgreSQL 并部署多个实例（见[多实例部署](#多实例部署)）。
- Linux 上若有大量客户端同时建连，可适当调大 `net.core.somaxconn`。

## 常见问题

<details>
<summary><b>本机用 curl 调用没有任何输出</b></summary>

多半是终端设置了 `http_proxy` 之类的代理环境变量，请求被发给了代理。加 `--noproxy '*'` 试一下；确认后，把 `127.0.0.1,localhost` 加进 `no_proxy`，或者在代理软件里让本机地址直连。
</details>

<details>
<summary><b>docker compose 构建时卡在 Docker Hub 或超时</b></summary>

国内网络访问 Docker Hub、`proxy.golang.org` 不稳定。把 `.env.example` 末尾的国内配置复制到 `.env`，或者先手动拉好基础镜像再构建。
</details>

<details>
<summary><b>Kimi Code 返回 403</b></summary>

Kimi Code 只接受编码工具的请求。供应商的 User-Agent 策略要保持“透传客户端”，并用 Claude Code 等实际客户端经网关调用；后台“测试”按钮的 UA 会被拒绝，属于正常现象。
</details>

<details>
<summary><b>从上游拉取不到模型列表</b></summary>

很多编码套餐不开放 `/models` 接口。在供应商表单里手动输入模型名，回车添加即可。
</details>

<details>
<summary><b>忘记管理令牌</b></summary>

设置 `ADMIN_TOKEN` 环境变量后重启，会以它为准。
</details>

<details>
<summary><b>Codex 报 previous_response_id 不支持</b></summary>

只在上游不是 Responses API 时出现。Codex 默认 `store: false` 并发送完整历史，不会触发；如果你的客户端依赖服务端会话，请把模型映射到支持 Responses API 的上游。
</details>

## 管理 API

控制台的所有操作都可以通过 `/admin/api/*` 完成（`Authorization: Bearer <ADMIN_TOKEN>`），例如：

```bash
curl -H "Authorization: Bearer $ADMIN_TOKEN" http://127.0.0.1:8080/admin/api/providers
curl -H "Authorization: Bearer $ADMIN_TOKEN" "http://127.0.0.1:8080/admin/api/stats?range=24h"
```

常用端点：`providers`、`models`、`keys`、`logs`、`stats`、`status`、`settings`、`alerts`、`captures`、`export`、`import`。

## 监控

- **`GET /healthz`**：数据库可用时返回 `ok`，否则返回 503，可作为容器和负载均衡的健康检查。
- **`GET /metrics`**：Prometheus 文本格式的指标，需要管理令牌（Prometheus 的 `authorization: { credentials: <ADMIN_TOKEN> }`）。指标按对外模型、上游和协议打标签，不包含请求内容：

| 指标 | 含义 |
|---|---|
| `ai_route_requests_total{model,provider,inbound,status}` | 请求数 |
| `ai_route_request_duration_seconds` / `ai_route_ttfb_seconds` | 耗时与首字节时间直方图 |
| `ai_route_tokens_total{kind=input\|cached\|output}`、`ai_route_cost_total{currency}` | 用量与费用 |
| `ai_route_upstream_attempts_total{target,result}`、`ai_route_fallbacks_total` | 每次上游尝试的结果、发生切换的请求数 |
| `ai_route_rejected_total{key,status}` | 被限流、预算、权限拒绝的请求 |
| `ai_route_breaker_open{kind,name}`、`ai_route_provider_inflight` | 冷却中的套餐 / 模型、自建模型的在途请求 |
| `ai_route_provider_quota_used_ratio{provider,period,kind}` | 套餐配额在当前窗口的用量占比，1 表示用满 |
| `ai_route_log_dropped_total` | 因日志队列持续满载而丢失的日志条数，正常应为 0 |

- **日志**：输出到标准错误，`LOG_FORMAT=json` 时每行一个 JSON 对象，字段包括 `request`（请求 ID）、`model`、`target`、`status`、`err` 等，可以和响应头 `X-Route-Request-Id` 对应。

## 开发

```bash
go test -race ./...      # 用模拟上游跑全部测试，不需要任何真实 Key
go build -o bin/ai-route .

# 存储相关测试再用 PostgreSQL 跑一遍（每个测试使用独立的 schema，结束后删除）
AI_ROUTE_TEST_DATABASE_URL='postgres://postgres:test@127.0.0.1:5432/airoute?sslmode=disable' \
  go test -race ./internal/store/... ./internal/gateway ./internal/admin
```

```
main.go               入口：参数、内嵌静态资源、HTTP 服务
internal/gateway      对外 API、路由与切换、熔断、限流、并发与健康检查、请求头处理
internal/convert      OpenAI Chat / Responses / Anthropic 之间的请求、响应、流式转换
internal/store        SQLite / PostgreSQL 存储、配置快照、请求日志与统计、数据迁移、多实例协调
internal/admin        管理 API
internal/alert        告警推送（飞书、钉钉、企业微信、Webhook）
internal/hdrtpl       请求头模板
web/                  控制台前端（原生 JS，无构建步骤；英文文案在 i18n.js）
```

测试覆盖协议互转（含流式事件顺序）、重试与熔断、模型映射与同级分流、限流与预算、费用计算、告警签名、请求头模板、自建模型的并发与健康检查等，全部使用模拟上游。

## 路线图

- [x] 英文 README 与控制台中英文切换
- [x] PostgreSQL 存储与数据迁移
- [ ] 发布预编译二进制和 Docker 镜像
- [ ] 多管理员账号与操作审计
- [x] 多实例部署（共享熔断与限流状态）

欢迎在 Issues 里提需求。

## 参与贡献

欢迎提交 Issue 和 Pull Request。提交前请确保：

- `gofmt -l .` 没有输出，`go vet ./...` 和 `go test -race ./...` 通过；
- 新功能附带测试（参照 `internal/gateway/*_test.go` 里的模拟上游写法）；
- 控制台新增文案用 `t()` 包起来，并在 `web/i18n.js` 里补上英文（`node scripts/check-i18n.mjs` 会检查）；
- 新增或修改供应商预设时，在 PR 里附上官方文档链接。

## 免责声明

- 部分编码套餐的条款限定只能在官方支持的编码工具中使用，或者禁止“自建后端 / 代理转发 / 自动化调用”（例如百炼 Coding Plan、GLM Coding Plan、Kimi Code 都有类似条款）。**通过本项目转发是否合规，请自行阅读并遵守各厂商的服务条款，由此产生的封号、扣费等后果由使用者自行承担。**
- 本项目与文中提到的任何厂商均无关联，预设信息仅供参考，以各厂商官方说明为准。

## 许可证

[MIT](LICENSE)
