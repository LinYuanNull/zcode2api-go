# 出站契约观察（A4 采样）

> 采集方式与工具见 [`README.md`](README.md)。这里只记**实测钉死的规则**与**未覆盖项**。
> 纪律同 A1/A2：**未覆盖的分支不凭猜测补全**，实现时显式报错。

## 一、上游主机与端点

| # | 事实 | 依据 |
|---|---|---|
| 1 | 上游主机只有两个：**`zcode.z.ai`**（配置 / 遥测）与 **`api.z.ai`**（消息转发） | MITM 捕获的 CONNECT 目标 |
| 2 | 消息转发端点 = `POST https://api.z.ai/api/anthropic/v1/messages` | `02-messages.POST.json` |
| 3 | 该端点 = `client/configs` 里 `providers[]` 中 `id=z-ai` 且 `schema=anthropic` 的 `baseUrl` + `/v1/messages` | 两处样本对照 |
| 4 | 客户端配置端点 = `GET https://zcode.z.ai/api/v1/client/configs?app_version=3.14.4` | `01-client-configs.GET.json` |
| 5 | 遥测端点 = `POST https://zcode.z.ai/api/v1/event/report` | `03-event-report.POST.json` |
| 6 | **出站地址硬编码**：10 个候选环境变量（`ZCODE_UPSTREAM_BASE` 等）实测**全部不生效** | `D:/tmp/probe_upstream_base.py` 的端口分派实验 |

## 二、出站走代理（这是 MITM 可行的原因）

| # | 事实 | 依据 |
|---|---|---|
| 7 | `/v1/messages` 的**出站走 `HTTPS_PROXY`** | 代理收到 `CONNECT zcode.z.ai:443` / `CONNECT api.z.ai:443` |
| 8 | 出站客户端信任 `SSL_CERT_FILE`（httpx `trust_env=True` 语义）⇒ 设成本地 CA 即可解密 | 设 `SSL_CERT_FILE=<自签 CA>` 后 MITM 成功终结 TLS |

## 三、入站 → 出站的字段映射（已实测部分）

| # | 事实 | 依据 |
|---|---|---|
| 9 | `messages[].content` 为**字符串**时，出站被包装成 `[{"type":"text","text":<原串>}]` | `02-messages.POST.json` 的 `body_inbound` / `body_outbound` |
| 10 | `model` / `max_tokens` **原样**透传 | 同上 |
| 11 | 出站鉴权头是 **`X-Api-Key`**（不是 `Authorization`），值为账号 token 明文 | 同上 |
| 12 | 固定头：`Anthropic-Version: 2023-06-01`、`User-Agent: ZCode/3.14.4`、`X-Zcode-Agent: glm`、`X-Zcode-App-Version: 3.14.4`、`Http-Referer: https://zcode.z.ai` | 同上 |
| 13 | 上游错误体**原样透传**：`{"error":{"message":"token expired or incorrect","type":"401"}}`，`type` 是**状态码字符串** | 同上 |
| 14 | `client/configs` **不带任何鉴权头**（公开目录） | `01-client-configs.GET.json` |
| 15 | `client/configs` 响应 **gzip 压缩**，出站客户端必须解压 | 捕获记录的 `Content-Encoding: gzip` |
| 16 | 遥测 `event/report` 的 `User-Agent` 是 `python-httpx/0.28.1`（与消息转发不同） | `03-event-report.POST.json` |
| 17 | 遥测 body 的 `client_timezone` / `client_language` / `screen_resolution` / `device_os_category` / `device_os_version` / `device_mid` **全部取自账号指纹** | 同一次运行内两次上报用了两个不同账号的指纹 |

## 四、模型表：`/v1/models` 的来源（**修正 A3 的一处假设**）

| # | 事实 | 依据 |
|---|---|---|
| 18 | 入站 `GET /v1/models` 返回 `GLM-5.3` / `GLM-5.3-Flash`，**恰好等于** `client/configs` 的 `data.builtinModels[].modelId` | `01-client-configs.GET.json` + `gateway/23-models-ok.GET.json` |
| 19 | 因此 A1 记录的「模型表为编译期常量（`app/constants.AVAILABLE_MODELS`）」**只是现象的一种解释**；至少存在「由 `builtinModels` 派生」的路径 | 同上 |

> **待钉死**：把 `client/configs` 请求阻断（代理返回错误）后 `/v1/models` 是否回落到内置常量。
> 这决定 Go 实现要不要内嵌兜底表。**未测之前不猜** —— 已登记为 A4 待办。

## 五、调度行为（实测，尚未完全解释）

| # | 事实 | 依据 |
|---|---|---|
| 20 | 新增的 apiKey 账号初始 `status=active` ⇒ `quota_pool` 计入 ⇒ **会真的发起出站转发** | `02-accounts-one.GET.json` + 实测池计数 0→1 |
| 21 | **一次入站请求会消耗多个账号**：实测池计数 `13 → 9 → 4 → 0`（≈4–5 个/请求） | 采样脚本打印的 `pool=` |
| 22 | 账号首次转发失败（401）后转 `invalid`，后续请求不再用它 | 池计数下降 + 账号 `recent_results` |
| 23 | **每次尝试前都会拉一次 `client/configs`**（单次请求内可看到多次 configs 抓取） | 捕获记录里 configs 的条数远多于请求数 |
| 24 | **`model` 不在目录内时也可能不出站**（`glm-4.6` 不在 `providers[].models`，`GLM-5.3` 在 `builtinModels`） | 不同 model 的多次采样结果不一致 |

> **未解释**：为什么部分请求**完全没有 `api.z.ai` 出站**（池被消耗但无转发）。
> 可能是「先用 configs 解析模型 → 解析失败则跳过账号」或「需要先 install/注册」。
> **A4 实现前必须把这条钉死** —— 否则「逐项一致」无从谈起。

## 六、未覆盖（**不凭猜测补全**）

- **200 成功响应体**与 **SSE 分块**（`stream:true` 的真实回执）—— 需真实账号。
- `system` / `tools` / `tool_choice` / `metadata` / `stop_sequences` / `temperature` 等字段的**变换规则**。
- `content` 为**数组**时的行为（是否原样透传）。
- `messages` 多轮 / `assistant` 角色的变换。
- 上游 **429 / 5xx / 风控 3012 / 验证码挑战** 的真实回执与冷却时长。
- `client/configs` 的 `builtin_provider_config_json`（指向 CDN 的二次配置）内容。
