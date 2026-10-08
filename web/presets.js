'use strict';

// Provider preset catalog for the "add provider" dialog.
//
// Fields:
//   id         stable identifier, saved as provider.vendor
//   category   coding (subscription coding plans) | official (pay-as-you-go
//              vendor APIs) | aggregator (multi-vendor platforms / relays)
//   prefix     default model prefix
//   openai     OpenAI-compatible base URL incl. version segment ('' = none)
//   anthropic  Anthropic-compatible base URL, i.e. ANTHROPIC_BASE_URL ('' = none)
//   models     common model ids (pre-filled; the upstream list is fetched too)
//   protocols  per-model protocol overrides (glob -> openai|anthropic)
//   rules      request body rules ({model, when?, protocol?, set}) merged upstream
//   headers    extra request headers the vendor asks for
//   ua         { mode: 'passthrough'|'override', value, note }
//   currency   currency the vendor bills in (default CNY), used for unit prices
//   verified   false = not fully confirmed from official docs
//
// Sources: vendor docs and the cc-switch preset files (checked 2026-10).
// Endpoints change over time: always check the vendor console.

const PRESET_CATEGORIES = [
  ['coding', '编码套餐'],
  ['official', '官方 API'],
  ['aggregator', '聚合平台'],
];
const CATEGORY_LABEL = Object.fromEntries(PRESET_CATEGORIES);

const PRESETS = [
  // ============================================================ coding plans
  {
    id: 'kimi-code', name: 'Kimi Code', category: 'coding', prefix: 'kimi', keywords: 'moonshot 月之暗面 kimi for coding 会员',
    openai: 'https://api.kimi.com/coding/v1', anthropic: 'https://api.kimi.com/coding',
    models: ['k3', 'k3-256k', 'kimi-for-coding', 'kimi-for-coding-highspeed'],
    docs: 'https://www.kimi.com/code/docs/', keyUrl: 'https://www.kimi.com/code/console',
    note: '随 Kimi 会员额度（5 小时 / 周 / 月）。社区规范限个人交互式使用，禁止通过反向代理共享账号或转售；和 Moonshot 开放平台是两套 Key。',
    ua: { mode: 'passthrough', note: '只放行编码工具（实测 claude-cli、claude-code、Kilo-Code 可用）；官方禁止篡改 User-Agent，违者可能暂停权益。请保持“透传客户端”。' },
  },
  {
    id: 'kimi-code-global', currency: 'USD', name: 'Kimi Code（海外）', category: 'coding', prefix: 'kimi-intl', keywords: 'moonshot kimi global international',
    openai: 'https://api.kimi.ai/coding/v1', anthropic: 'https://api.kimi.ai/coding',
    models: ['k3', 'k3-256k', 'kimi-for-coding', 'kimi-for-coding-highspeed'],
    docs: 'https://www.kimi.com/code/docs/en/', keyUrl: 'https://www.kimi.ai/code',
    note: '海外站，规则同国内 Kimi Code。',
    ua: { mode: 'passthrough', note: '只放行编码工具，禁止篡改 User-Agent。' },
  },
  {
    id: 'glm-coding', name: '智谱 GLM Coding Plan', category: 'coding', prefix: 'glm', keywords: 'zhipu bigmodel glm',
    openai: 'https://open.bigmodel.cn/api/coding/paas/v4', anthropic: 'https://open.bigmodel.cn/api/anthropic',
    models: ['glm-5.3', 'glm-5.3-flash', 'glm-5.2', 'glm-5-turbo'],
    docs: 'https://docs.bigmodel.cn/cn/coding-plan/tool/others', keyUrl: 'https://open.bigmodel.cn/usercenter/proj-mgmt/apikeys',
    note: 'OpenAI 协议务必用 /api/coding/paas/v4，配成通用 /api/paas/v4 不扣套餐额度、按量计费。条款限官方列出的编码工具使用。',
  },
  {
    id: 'zai-coding', currency: 'USD', name: 'Z.ai GLM Coding Plan', category: 'coding', prefix: 'zai', keywords: 'glm zhipu international',
    openai: 'https://api.z.ai/api/coding/paas/v4', anthropic: 'https://api.z.ai/api/anthropic',
    models: ['glm-5.3', 'glm-5.3-flash', 'glm-5.2'],
    docs: 'https://docs.z.ai/devpack/tool/others', keyUrl: 'https://z.ai/manage-apikey/apikey-list',
    note: 'GLM Coding Plan 国际站，限官方支持的编码工具。',
  },
  {
    id: 'bailian-coding', name: '阿里云百炼 Coding Plan', category: 'coding', prefix: 'bailian', keywords: 'aliyun dashscope qwen 通义 阿里',
    openai: 'https://coding.dashscope.aliyuncs.com/v1', anthropic: 'https://coding.dashscope.aliyuncs.com/apps/anthropic',
    models: ['qwen3.7-plus', 'qwen3.6-plus', 'qwen3-coder-plus', 'glm-5', 'kimi-k2.5', 'MiniMax-M2.5'],
    docs: 'https://help.aliyun.com/zh/model-studio/coding-plan', keyUrl: 'https://bailian.console.aliyun.com',
    note: 'Key 以 sk-sp- 开头，和按量 Key / 地址不能混用。条款限编程工具使用，禁止脚本、自建后端和批量调用。',
  },
  {
    id: 'qwencloud-coding', currency: 'USD', name: 'QwenCloud Coding（国际）', category: 'coding', prefix: 'qwencloud', keywords: 'aliyun qwen international',
    openai: 'https://coding-intl.dashscope.aliyuncs.com/v1', anthropic: 'https://coding-intl.dashscope.aliyuncs.com/apps/anthropic',
    models: ['qwen3.7-plus', 'qwen3.6-plus', 'qwen3-coder-plus'],
    docs: 'https://www.qwencloud.com', keyUrl: 'https://home.qwencloud.com/api-keys',
    note: '阿里云 Coding Plan 国际站。',
  },
  {
    id: 'qianwen-tokenplan', name: '千问 Token Plan', category: 'coding', prefix: 'qianwen', keywords: 'aliyun qwen token plan 阿里',
    openai: 'https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1', anthropic: 'https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic',
    models: ['qwen3.8-max', 'qwen3.7-plus', 'qwen3.8-flash'],
    docs: 'https://platform.qianwenai.com/pricing/token-plan', keyUrl: 'https://platform.qianwenai.com/home/api-keys',
    note: '订阅制。国际站把域名换成 token-plan.ap-southeast-1.maas.aliyuncs.com。',
  },
  {
    id: 'volc-coding', name: '火山方舟 Coding Plan', category: 'coding', prefix: 'volc', keywords: 'volcengine ark 字节 doubao 豆包',
    openai: 'https://ark.cn-beijing.volces.com/api/coding/v3', anthropic: 'https://ark.cn-beijing.volces.com/api/coding',
    models: ['ark-code-latest'],
    docs: 'https://www.volcengine.com/docs/82379/1928261', keyUrl: 'https://console.volcengine.com/ark/region:ark+cn-beijing/apiKey',
    note: '不要用 /api/v3（不扣套餐、另行计费）。额度仅在 AI 编程工具中生效，滥用可能封号。',
  },
  {
    id: 'volc-agentplan', name: '火山方舟 Agent Plan', category: 'coding', prefix: 'volc-agent', keywords: 'volcengine ark agent plan',
    openai: 'https://ark.cn-beijing.volces.com/api/plan/v3', anthropic: 'https://ark.cn-beijing.volces.com/api/plan',
    models: ['ark-code-latest'],
    docs: 'https://docs.volcengine.com/docs/82379/2556054', keyUrl: 'https://www.volcengine.com/activity/agentplan',
    note: '和 Coding Plan 是不同订阅，地址和 Key 不能互换。',
  },
  {
    id: 'byteplus-coding', currency: 'USD', name: 'BytePlus ModelArk Coding Plan', category: 'coding', prefix: 'byteplus', keywords: 'volcengine ark international',
    openai: 'https://ark.ap-southeast.bytepluses.com/api/coding/v3', anthropic: 'https://ark.ap-southeast.bytepluses.com/api/coding',
    models: ['ark-code-latest'],
    docs: 'https://www.byteplus.com/en/product/modelark', keyUrl: 'https://www.byteplus.com/en/product/modelark',
    note: '火山方舟国际站。',
  },
  {
    id: 'minimax-tokenplan', name: 'MiniMax Token Plan', category: 'coding', prefix: 'minimax', keywords: 'minimax coding plan',
    openai: 'https://api.minimax.cn/v1', anthropic: 'https://api.minimax.cn/anthropic',
    models: ['MiniMax-M3', 'MiniMax-M2.7'],
    docs: 'https://platform.minimax.cn/docs/token-plan/other-tools', keyUrl: 'https://platform.minimax.cn/subscribe/token-plan',
    note: '订阅 Key（sk-cp-）和按量 Key 不通用；旧域名 api.minimaxi.com。',
  },
  {
    id: 'minimax-tokenplan-intl', currency: 'USD', name: 'MiniMax Token Plan（国际）', category: 'coding', prefix: 'minimax-intl', keywords: 'minimax international',
    openai: 'https://api.minimax.io/v1', anthropic: 'https://api.minimax.io/anthropic',
    models: ['MiniMax-M3'],
    docs: 'https://platform.minimax.io/docs/token-plan/quickstart', keyUrl: 'https://platform.minimax.io/subscribe/coding-plan',
    note: '订阅 Key 和按量 Key 不通用。',
  },
  {
    id: 'opencode-go', currency: 'USD', name: 'OpenCode Go', category: 'coding', prefix: 'opencode', keywords: 'opencode zen go',
    openai: 'https://opencode.ai/zen/go/v1', anthropic: 'https://opencode.ai/zen/go',
    models: ['glm-5.3', 'kimi-k3', 'deepseek-v4-pro', 'deepseek-v4-flash', 'minimax-m3', 'qwen3.8-max'],
    protocols: {
      'glm-*': 'openai', 'kimi-*': 'openai', 'deepseek-*': 'openai', 'mimo-*': 'openai', 'longcat-*': 'openai', 'hy3': 'openai',
      'minimax-*': 'anthropic', 'qwen*': 'anthropic',
    },
    // the caller's own session id, else a stable id per conversation
    headers: { 'x-opencode-session': '{{header.x-opencode-session ?? $conversation}}' },
    docs: 'https://opencode.ai/docs/go/', keyUrl: 'https://opencode.ai/go',
    note: '月订阅。MiniMax / Qwen 只走 /messages，GLM / Kimi / DeepSeek / MiMo 走 chat，已预填“模型协议规则”；官方要求每个会话带稳定的 x-opencode-session 头：已预填为调用方传了就透传，没传由网关按会话生成。',
    ua: { mode: 'passthrough', note: '官方要求客户端用自己的 UA（如 my-agent/1.0），不要用 SDK / HTTP 库的默认 UA。透传客户端即可；客户端没带 UA 时使用平台标识 ai-route/版本号。' },
  },
  {
    id: 'stepfun-plan', name: '阶跃 Step Plan', category: 'coding', prefix: 'step', keywords: 'stepfun 阶跃星辰',
    openai: 'https://api.stepfun.com/step_plan/v1', anthropic: 'https://api.stepfun.com/step_plan',
    models: ['step-3.7-flash', 'step-3.5-flash-2603', 'step-5-preview', 'step-router-v1'],
    docs: 'https://platform.stepfun.com/docs/zh/step-plan/overview', keyUrl: 'https://platform.stepfun.com/interface-key',
    note: '额度独立，用普通 API 地址不扣套餐。国际站为 api.stepfun.ai/step_plan。',
  },
  {
    id: 'tencent-tokenplan', name: '腾讯云 Token Plan', category: 'coding', prefix: 'tencent', keywords: 'tencent lkeap hunyuan 腾讯',
    openai: 'https://api.lkeap.cloud.tencent.com/plan/v3', anthropic: 'https://api.lkeap.cloud.tencent.com/plan/anthropic',
    models: ['tc-code-latest', 'deepseek-v4-pro-202606', 'glm-5.1', 'minimax-m2.7'],
    docs: 'https://cloud.tencent.com/document/product/1823/130060', keyUrl: 'https://console.cloud.tencent.com/tokenhub/tokenplan',
    note: '订阅 Key 只能走 /plan；腾讯云 Coding Plan 则是 /coding/v3 和 /coding/anthropic。',
  },
  {
    id: 'qianfan-tokenplan', name: '百度千帆 Token Plan', category: 'coding', prefix: 'qianfan', keywords: 'baidu 百度 coding plan',
    openai: 'https://qianfan.baidubce.com/v2/tokenplan/personal', anthropic: 'https://qianfan.baidubce.com/anthropic/tokenplan/personal',
    models: ['deepseek-v4-pro', 'deepseek-v4-flash', 'glm-5.2', 'kimi-k2.6'],
    docs: 'https://cloud.baidu.com/product/codingplan.html', keyUrl: 'https://console.bce.baidu.com/qianfan/resource/token-plan',
    note: '2026-07-13 起 Token Plan 替代 Coding Plan（旧地址 /v2/coding、/anthropic/coding，模型 qianfan-code-latest）；需要专属 Key。',
  },
  {
    id: 'kat-coder', name: 'KAT-Coder（StreamLake）', category: 'coding', prefix: 'kat', keywords: 'kuaishou 快手 streamlake',
    openai: 'https://vanchin.streamlake.ai/api/gateway/v1/endpoints/${ENDPOINT_ID}/openai', anthropic: 'https://vanchin.streamlake.ai/api/gateway/v1/endpoints/${ENDPOINT_ID}/claude-code-proxy',
    models: ['KAT-Coder-Pro V1', 'KAT-Coder-Air V1'],
    docs: 'https://console.streamlake.ai', keyUrl: 'https://console.streamlake.ai/console/api-key',
    note: '需要先在控制台创建推理点，再把地址里的 ${ENDPOINT_ID} 替换成你的推理点 ID。',
  },
  // ============================================================ official APIs (pay-as-you-go)
  {
    id: 'moonshot', name: 'Kimi 开放平台（Moonshot）', category: 'official', prefix: 'moonshot', keywords: 'kimi 月之暗面 moonshot',
    openai: 'https://api.moonshot.cn/v1', anthropic: 'https://api.moonshot.cn/anthropic',
    models: ['kimi-k3', 'kimi-k2.7-code', 'kimi-k2.7-code-highspeed', 'kimi-k2.6'],
    docs: 'https://platform.kimi.com/docs/api/overview', keyUrl: 'https://platform.kimi.com/console/api-keys',
    note: '按量计费。Key 和站点绑定，.cn 与 .ai 不通用。若你买的是 Kimi 会员编码套餐，请选“Kimi Code”。',
  },
  {
    id: 'moonshot-global', currency: 'USD', name: 'Kimi Platform（海外）', category: 'official', prefix: 'moonshot-intl', keywords: 'kimi moonshot global',
    openai: 'https://api.moonshot.ai/v1', anthropic: 'https://api.moonshot.ai/anthropic',
    models: ['kimi-k3', 'kimi-k2.7-code', 'kimi-k2.7-code-highspeed', 'kimi-k2.6'],
    docs: 'https://platform.kimi.ai/docs/api/overview', keyUrl: 'https://platform.kimi.ai/console/api-keys',
    note: '按量计费，海外站。',
  },
  {
    id: 'zhipu', name: '智谱开放平台（按量）', category: 'official', prefix: 'zhipu', keywords: 'glm bigmodel',
    openai: 'https://open.bigmodel.cn/api/paas/v4', anthropic: 'https://open.bigmodel.cn/api/anthropic',
    models: ['glm-5.3', 'glm-5.3-flash', 'glm-5.2'],
    docs: 'https://docs.bigmodel.cn/cn/guide/develop/claude/introduction', keyUrl: 'https://open.bigmodel.cn/usercenter/proj-mgmt/apikeys',
    note: '按量计费。若你买的是 GLM Coding Plan，请选“智谱 GLM Coding Plan”。',
  },
  {
    id: 'zai', currency: 'USD', name: 'Z.ai（按量）', category: 'official', prefix: 'zai-api', keywords: 'glm zhipu international',
    openai: 'https://api.z.ai/api/paas/v4', anthropic: 'https://api.z.ai/api/anthropic',
    models: ['glm-5.3', 'glm-5.2'],
    docs: 'https://docs.z.ai', keyUrl: 'https://z.ai/manage-apikey/apikey-list',
    note: '国际站按量计费。', verified: false,
  },
  {
    id: 'dashscope', name: '阿里云百炼（按量）', category: 'official', prefix: 'dashscope', keywords: 'aliyun qwen bailian 通义 阿里',
    openai: 'https://dashscope.aliyuncs.com/compatible-mode/v1', anthropic: 'https://dashscope.aliyuncs.com/apps/anthropic',
    models: ['qwen3.8-max', 'qwen3.7-plus', 'qwen3.8-flash', 'qwen3.7-max'],
    // Qwen3 open-weight models think by default and reject non-stream calls unless thinking is off
    rules: [{ model: 'qwen3-*', when: 'nonstream', protocol: 'openai', set: { enable_thinking: false } }],
    docs: 'https://help.aliyun.com/zh/model-studio/', keyUrl: 'https://bailian.console.aliyun.com/?tab=model#/api-key',
    note: '按量计费。国际站把域名换成 dashscope-intl.aliyuncs.com。若你买的是 Coding Plan，请选“阿里云百炼 Coding Plan”。',
  },
  {
    id: 'ark', name: '火山方舟（按量）', category: 'official', prefix: 'ark', keywords: 'volcengine doubao 豆包 字节',
    openai: 'https://ark.cn-beijing.volces.com/api/v3', anthropic: 'https://ark.cn-beijing.volces.com/api/compatible',
    models: ['doubao-seed-2-1-pro-260628'],
    docs: 'https://www.volcengine.com/docs/82379', keyUrl: 'https://console.volcengine.com/ark/region:ark+cn-beijing/apiKey',
    note: '按量计费。部分模型需要用推理接入点 ID（ep-xxxx）作为模型名。',
  },
  {
    id: 'deepseek', name: 'DeepSeek', category: 'official', prefix: 'deepseek',
    openai: 'https://api.deepseek.com/v1', anthropic: 'https://api.deepseek.com/anthropic',
    models: ['deepseek-v4-pro', 'deepseek-flash'],
    docs: 'https://api-docs.deepseek.com/guides/anthropic_api', keyUrl: 'https://platform.deepseek.com/api_keys',
    note: '按量计费。Anthropic 端会把未知模型名自动映射到 deepseek-flash。',
  },
  {
    id: 'minimax', name: 'MiniMax 开放平台（按量）', category: 'official', prefix: 'minimax-api', keywords: 'minimax',
    openai: 'https://api.minimax.cn/v1', anthropic: 'https://api.minimax.cn/anthropic',
    models: ['MiniMax-M3'],
    docs: 'https://platform.minimax.cn/docs', keyUrl: 'https://platform.minimax.cn',
    note: '和 Token Plan 同一地址、不同 Key；国际站换成 api.minimax.io。', verified: false,
  },
  {
    id: 'stepfun', name: '阶跃星辰（按量）', category: 'official', prefix: 'stepfun', keywords: 'stepfun step',
    openai: 'https://api.stepfun.com/v1', anthropic: '',
    models: ['step-3.7-flash'],
    docs: 'https://platform.stepfun.com/docs', keyUrl: 'https://platform.stepfun.com/interface-key',
    note: '按量计费，暂无 Anthropic 兼容端点。',
  },
  {
    id: 'hunyuan', name: '腾讯混元 TokenHub（按量）', category: 'official', prefix: 'hunyuan', keywords: 'tencent 腾讯 hy3',
    openai: 'https://tokenhub.tencentmaas.com/v1', anthropic: '',
    models: ['hy3', 'hy4-preview'],
    docs: 'https://cloud.tencent.com/document/product/1823/133532', keyUrl: 'https://console.cloud.tencent.com/tokenhub/apikey',
    note: '需要 TokenHub Key（勾选 Hy3 范围），订阅 Key 不通用。',
  },
  {
    id: 'mimo', name: '小米 MiMo', category: 'official', prefix: 'mimo', keywords: 'xiaomi 小米',
    openai: 'https://api.xiaomimimo.com/v1', anthropic: 'https://api.xiaomimimo.com/anthropic',
    models: ['mimo-v2.6-pro', 'mimo-v2.6-flash', 'mimo-v2.5-pro'],
    docs: 'https://platform.xiaomimimo.com', keyUrl: 'https://platform.xiaomimimo.com/#/console/api-keys',
    note: '按量计费。Token Plan 地址为 token-plan-cn.xiaomimimo.com/v1 和 /anthropic。',
  },
  {
    id: 'longcat', name: '美团 LongCat', category: 'official', prefix: 'longcat', keywords: 'meituan 美团',
    openai: 'https://api.longcat.chat/openai/v1', anthropic: 'https://api.longcat.chat/anthropic',
    models: ['LongCat-2.0'],
    docs: 'https://longcat.chat/platform', keyUrl: 'https://longcat.chat/platform/api_keys',
  },
  {
    id: 'openai', currency: 'USD', name: 'OpenAI', category: 'official', prefix: 'openai', keywords: 'gpt',
    openai: 'https://api.openai.com/v1', anthropic: '',
    models: ['gpt-6-astra', 'gpt-6.1-sol', 'gpt-6-luna', 'gpt-5.6-sol'],
    docs: 'https://developers.openai.com/api/docs/models', keyUrl: 'https://platform.openai.com/api-keys',
    note: '按量计费，走 Chat Completions 接口。',
  },
  {
    id: 'anthropic', currency: 'USD', name: 'Anthropic', category: 'official', prefix: 'anthropic', keywords: 'claude',
    openai: '', anthropic: 'https://api.anthropic.com',
    models: ['claude-opus-5-5', 'claude-sonnet-5-5', 'claude-fable-5-1', 'claude-haiku-4-5'],
    docs: 'https://platform.claude.com/docs/en/about-claude/models/overview', keyUrl: 'https://platform.claude.com/settings/keys',
    note: '按量计费。官方的 OpenAI 兼容层仅供测试、不支持提示词缓存，所以只配置 Anthropic 端点（OpenAI 请求由网关转换）。',
  },
  {
    id: 'gemini', currency: 'USD', name: 'Google Gemini', category: 'official', prefix: 'gemini', keywords: 'google',
    openai: 'https://generativelanguage.googleapis.com/v1beta/openai', anthropic: '',
    models: ['gemini-3.8-flash', 'gemini-3.7-flash', 'gemini-3.1-pro-preview'],
    docs: 'https://ai.google.dev/gemini-api/docs/openai', keyUrl: 'https://aistudio.google.com/app/apikey',
    note: '使用 Gemini 的 OpenAI 兼容接口。',
  },
  {
    id: 'xai', currency: 'USD', name: 'xAI Grok', category: 'official', prefix: 'xai', keywords: 'grok',
    openai: 'https://api.x.ai/v1', anthropic: '',
    models: ['grok-4.7', 'grok-4.6', 'grok-4.5'],
    docs: 'https://docs.x.ai/developers/models', keyUrl: 'https://console.x.ai',
    note: '据报道其 Anthropic 兼容接口已弃用，只配置 OpenAI 端点。',
  },
  // ============================================================ aggregators / relays
  {
    id: 'opencode-zen', currency: 'USD', name: 'OpenCode Zen', category: 'aggregator', prefix: 'zen', keywords: 'opencode',
    openai: 'https://opencode.ai/zen/v1', anthropic: 'https://opencode.ai/zen',
    models: ['claude-opus-5-5', 'claude-sonnet-5', 'glm-5.3', 'kimi-k3'],
    protocols: { 'claude-*': 'anthropic', 'qwen*': 'anthropic' },
    docs: 'https://opencode.ai/docs/zen/', keyUrl: 'https://opencode.ai/auth',
    note: '按量计费。Claude / Qwen 走 /messages，其余走 chat；GPT / Grok（只有 Responses 接口）和 Gemini 暂不支持经网关调用。',
  },
  {
    id: 'openrouter', currency: 'USD', name: 'OpenRouter', category: 'aggregator', prefix: 'openrouter',
    openai: 'https://openrouter.ai/api/v1', anthropic: 'https://openrouter.ai/api',
    models: ['anthropic/claude-opus-5.5', 'anthropic/claude-sonnet-5', 'openai/gpt-5.6-sol', 'moonshotai/kimi-k3'],
    docs: 'https://openrouter.ai/docs', keyUrl: 'https://openrouter.ai/keys',
    note: '模型名本身带厂商前缀，映射里显示为 openrouter/anthropic/claude-sonnet-5。',
  },
  {
    id: 'siliconflow', name: '硅基流动 SiliconFlow', category: 'aggregator', prefix: 'siliconflow', keywords: 'siliconflow 硅基',
    openai: 'https://api.siliconflow.cn/v1', anthropic: 'https://api.siliconflow.cn',
    models: ['deepseek-ai/DeepSeek-V4-Flash', 'Pro/MiniMaxAI/MiniMax-M2.5', 'moonshotai/Kimi-K2-Instruct-0905'],
    docs: 'https://docs.siliconflow.cn/cn/usercases/use-siliconcloud-in-ClaudeCode', keyUrl: 'https://cloud.siliconflow.cn/account/ak',
    note: '按量计费。国际站为 api.siliconflow.com，Key 不通用。',
  },
  {
    id: 'modelscope', name: '魔搭 ModelScope', category: 'aggregator', prefix: 'modelscope', keywords: 'modelscope 阿里 魔搭',
    openai: 'https://api-inference.modelscope.cn/v1', anthropic: 'https://api-inference.modelscope.cn',
    models: ['ZhipuAI/GLM-5.2', 'Qwen/Qwen3-Coder-480B-A35B-Instruct'],
    docs: 'https://modelscope.cn/docs/model-service/API-Inference/intro', keyUrl: 'https://modelscope.cn/my/myaccesstoken',
    note: 'Key 为 SDK Token（ms- 开头），每日有免费调用额度。',
  },
  {
    id: 'novita', currency: 'USD', name: 'Novita AI', category: 'aggregator', prefix: 'novita',
    openai: 'https://api.novita.ai/openai/v1', anthropic: 'https://api.novita.ai/anthropic',
    models: ['zai-org/glm-5.3', 'moonshotai/kimi-k3'],
    docs: 'https://novita.ai/docs', keyUrl: 'https://novita.ai/settings/key-management',
  },
  {
    id: 'aihubmix', name: 'AiHubMix', category: 'aggregator', prefix: 'aihubmix',
    openai: 'https://aihubmix.com/v1', anthropic: 'https://aihubmix.com',
    models: ['claude-opus-5-5', 'claude-sonnet-5', 'gpt-5.6-sol'],
    docs: 'https://aihubmix.com', keyUrl: 'https://aihubmix.com',
    note: '备用地址 api.aihubmix.com。',
  },
  {
    id: 'packycode', name: 'PackyCode', category: 'aggregator', prefix: 'packy',
    openai: 'https://www.packyapi.ai/v1', anthropic: 'https://www.packyapi.ai',
    models: ['claude-sonnet-5', 'claude-opus-5', 'gpt-5.6-sol'],
    docs: 'https://www.packyapi.ai', keyUrl: 'https://www.packyapi.ai/register',
    note: '中转站，可用模型由分组决定。',
  },
];

const PRESET_INDEX = Object.fromEntries(PRESETS.map((p) => [p.id, p]));
function presetById(id) { return (id && PRESET_INDEX[id]) || null; }

// guessVendor matches providers created before presets were recorded.
function guessVendor(p) {
  if (p.vendor) return p.vendor;
  const hit = PRESETS.find((ps) => (p.openai_base_url && p.openai_base_url === ps.openai) || (p.anthropic_base_url && p.anthropic_base_url === ps.anthropic));
  return hit ? hit.id : '';
}

// ------------------------------------------------------------ model tags
// Capability tags shown on public models. `single` groups allow one value.
const TAG_GROUPS = [
  { id: 'type', label: '类型', items: [
    ['chat', '文本对话', '对话 / 补全，走 /v1/chat/completions 或 /v1/messages'],
    ['embedding', '向量', '文本向量化（Embedding），走 /v1/embeddings'],
    ['rerank', '重排序', '检索结果重排（Rerank）'],
    ['image', '图像生成', '文生图 / 图生图'],
    ['audio', '语音', '语音识别或语音合成'],
  ] },
  { id: 'ability', label: '能力', items: [
    ['vision', '多模态', '可以理解图片输入'],
    ['reasoning', '深度思考', '支持思考 / 推理模式'],
    ['tools', '工具调用', '支持 Function Calling / Tool Use'],
    ['code', '编程', '针对编程场景优化'],
    ['json', '结构化输出', '支持 JSON 模式 / 结构化输出'],
    ['search', '联网搜索', '内置联网搜索'],
  ] },
  { id: 'context', label: '上下文', single: true, items: [
    ['ctx-32k', '32K', '上下文窗口约 32K tokens'],
    ['ctx-128k', '128K', '上下文窗口约 128K tokens'],
    ['ctx-200k', '200K', '上下文窗口约 200K tokens'],
    ['ctx-256k', '256K', '上下文窗口约 256K tokens'],
    ['ctx-1m', '1M', '上下文窗口约 1M tokens'],
  ] },
  { id: 'trait', label: '特点', items: [
    ['fast', '高速', '响应快 / 高速版'],
    ['cheap', '经济', '价格低，适合批量和简单任务'],
    ['free', '免费', '免费额度或免费模型'],
  ] },
];
const TAG_INDEX = {};
for (const g of TAG_GROUPS) for (const [id, label, desc] of g.items) TAG_INDEX[id] = { id, label, desc, group: g.id };
const TAG_ORDER = TAG_GROUPS.flatMap((g) => g.items.map(([id]) => id));

function tagInfo(t) { return TAG_INDEX[t] || { id: t, label: t, desc: '自定义标签', group: 'custom' }; }
function sortTags(tags) {
  return [...tags].sort((a, b) => {
    const ia = TAG_ORDER.indexOf(a), ib = TAG_ORDER.indexOf(b);
    return (ia < 0 ? 999 : ia) - (ib < 0 ? 999 : ib) || a.localeCompare(b);
  });
}

// Tag suggestion from an upstream model id. Conservative name heuristics:
// only families whose capabilities are well known; everything is a
// suggestion the admin confirms.
const CTX_RANK = { 'ctx-32k': 1, 'ctx-128k': 2, 'ctx-200k': 3, 'ctx-256k': 4, 'ctx-1m': 5 };
function suggestTagsFor(modelId) {
  const n = String(modelId).toLowerCase();
  const tags = new Set();
  if (/rerank/.test(n)) tags.add('rerank');
  else if (/embed|text-embedding|(^|[/-])bge[-_]|m3e|(^|[/-])gte-/.test(n)) tags.add('embedding');
  else if (/dall-e|gpt-image|flux|seedream|cogview|wanx|kolors|stable-diffusion|(^|[/-])sd-|imagen/.test(n)) tags.add('image');
  else if (/(^|[/-])tts|whisper|(^|[/-])asr|speech|cosyvoice|paraformer|audio/.test(n)) tags.add('audio');
  else tags.add('chat');
  if (tags.has('chat')) {
    if (/(^|[/-])vl($|[-.])|vision|omni|gpt-4o|claude|gemini|gpt-5|gpt-6|grok-4/.test(n)) tags.add('vision');
    if (/think|reason|(^|[/-])r1($|[-.])|qwq|claude-(opus|sonnet)|gemini-.*pro|gpt-5|gpt-6|(^|[/-])o[134]($|-)/.test(n)) tags.add('reasoning');
    if (/claude|gpt|gemini|glm|kimi|(^|[/-])k3|qwen|deepseek|minimax|grok|doubao|mimo|step-|longcat|ark-code|hy3/.test(n)) tags.add('tools');
    if (/code|coding|codex|codestral|devstral/.test(n)) tags.add('code');
    if (/flash|turbo|highspeed|fast|[-_]mini($|[-_.])|lite|(^|[/-])air|haiku|instant/.test(n)) tags.add('fast');
    if (/free/.test(n)) tags.add('free');
    if (/1m($|[^a-z0-9])|\[1m\]|gemini/.test(n) || /(^|[/-])k3$/.test(n)) tags.add('ctx-1m');
    else if (/256k/.test(n)) tags.add('ctx-256k');
    else if (/200k/.test(n)) tags.add('ctx-200k');
    else if (/128k/.test(n)) tags.add('ctx-128k');
    else if (/32k/.test(n)) tags.add('ctx-32k');
  }
  return tags;
}

// suggestTags combines the targets of a mapping: a tag is suggested only when
// every target has it (fallbacks must not silently lose a capability); the
// context is the smallest one. Returns { tags, partial } where partial lists
// tags only some targets have.
function suggestTags(targets) {
  const per = targets.map((t) => ({ t, tags: suggestTagsFor(t.slice(t.indexOf('/') + 1)) }));
  if (!per.length) return { tags: [], partial: [] };
  const all = new Set(per.flatMap((p) => [...p.tags]));
  const common = [...all].filter((x) => !x.startsWith('ctx-') && per.every((p) => p.tags.has(x)));
  const ctxs = per.map((p) => [...p.tags].find((x) => x.startsWith('ctx-')));
  if (ctxs.every(Boolean)) common.push(ctxs.sort((a, b) => CTX_RANK[a] - CTX_RANK[b])[0]);
  const partial = [...all].filter((x) => !common.includes(x) && !x.startsWith('ctx-'))
    .map((x) => ({ tag: x, targets: per.filter((p) => p.tags.has(x)).map((p) => p.t) }));
  return { tags: sortTags(common), partial };
}
