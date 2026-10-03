# 转发行为契约（A4 判据）

> 这份文件记的是**参考实现（上游 `dengyie/zcode2api`）在受控假上游下的可观察行为**。
> 采集方式：`python tools/behavior_diff.py`（`tools/mitmupstream --mode fake` 把上游响应
> 变成输入参数）。**每条都是实测**，不是从上游源码读来的，也不是推测。
>
> 与 `observations.md` 的分工：那边记「网关 → 上游」的**出站契约**（请求长什么样），
> 这边记「客户端 → 网关」的**入站行为**（各种上游响应下，客户端看到什么）。

## 一、两个入口的转发语义完全不同

| 入口 | 出站 body | 入站响应 |
|---|---|---|
| `POST /v1/messages` | **保序改写**：只把 `messages[].content` 的字符串包装成 `[{type:text,text:…}]`，其余字段原样 | **上游响应逐字节透传**（状态码 + body + `Content-Type` 都照搬） |
| `POST /v1/chat/completions` | **重建**：键序固定 `model, messages, max_tokens`（+ `stream` 追加在末尾） | **必须解析**：转成 OpenAI 形状（JSON）或 OpenAI SSE；解析失败 → 502 |

**`/v1/messages` 不解析响应**，所以上游给什么就回什么。而且**与入站的 `stream` 字段完全无关**
（探针 `m-absent__sse` / `m-false__sse`：入站没有 / 显式 `stream:false`，上游给 SSE 照样原样回 SSE）：

| 上游响应 | 入站 |
|---|---|
| 200 + JSON | 200 + **同样的字节**，`Content-Type: application/json` |
| 200 + SSE（无论入站 `stream` 是什么） | 200 + **同样的字节**，`Content-Type: text/event-stream; charset=utf-8` |
| 200 + 非 JSON 文本（`not json at all`） | 200 + `not json at all` |
| 200 + 错误体 `{"error":…}` | 200 + 同样的字节 |
| 400 | 400 + 上游错误体逐字透传 |

**响应头规则（实测，探针 `h-*`）**：

| 项 | 行为 |
|---|---|
| `Content-Type` | **照搬上游**（上游 `text/plain` → 入站 `text/plain; charset=utf-8`；上游 `text/x-probe` → 入站 `text/x-probe; charset=utf-8`）。`text/*` 追加 `; charset=utf-8` 是 uvicorn/Starlette 的行为。 |
| 其它上游响应头 | **一律不透传**（探针给上游加了 `X-Custom-Probe: abc`，入站没有它）。 |
| `cache-control: no-cache` | 正常响应路径**总是**加上（客户端错透传分支除外，见下行）。 |
| `server` / `date` / `transfer-encoding` | uvicorn 自己加的，不是实现的行为。 |
| 「客户端错」透传分支的响应头 | **只有** Content-Type（照搬+charset 规则同上），**没有** `cache-control` —— no-cache 只加在正常响应路径（harness `upstream-400` 对照实测，2026-10-04）。 |
| `/v1/chat/completions` **非流式** 200 | `Content-Type: application/json`（**不是**照搬上游），**没有** `cache-control`（harness `ok-openai` 对照实测：靶机非流式 chat 响应只有 content-type）。 |
| `/v1/chat/completions` **流式** 200 | `Content-Type: text/event-stream; charset=utf-8` + `cache-control: no-cache`（harness `ok-openai-stream`）。 |
| `/v1/chat/completions` **502** | `Content-Type: application/json`，**没有** `cache-control`（harness `chat-sse-on-nonstream`）。 |

**`/v1/chat/completions` 必须解析**（`stream` 用**真值**判定，见 §3.0 末）：

| 上游响应 | 入站 |
|---|---|
| 200 + Anthropic JSON | 200 + 转换后的 OpenAI JSON |
| 200 + SSE（入站 `stream` 为真值） | 200 + 转换后的 OpenAI SSE |
| 200 + JSON（入站 `stream` 为真值） | 200 + **转换后的 OpenAI SSE**（探针 `c-true__json`） |
| 200 + SSE（入站 `stream` 为**假值**） | **502** `{"error":{"message":"上游响应格式异常","type":"upstream_error"}}` |
| 200 + 非 JSON 文本（入站 `stream` 为假值） | 同上 502 |

> ⚠️ **修正**：此前本表把「200 + SSE（入站非流式）→ 502」挂在 `/v1/messages` 上，**是错的**。
> 探针 `m-absent__sse`（`/v1/messages` + 无 `stream` + 上游 SSE）实测是 **200 原样透传 SSE**。
> 502 只发生在 **`/v1/chat/completions`** 且入站 `stream` 为假值时。
> `tools/behavior_diff.py` 的 `sse-to-nonstream` 场景已按此改名/改路径（见该文件）。

**流式的「到达时机」**（不在 harness 的字节对照范围内，但是真流式的硬要求）：
`/v1/messages` 透传与 `/v1/chat/completions` 转换**都必须逐写 flush** —— 上游每段数据
到达即写出，不允许攒到 net/http 的响应缓冲（默认 2 KB）里。参考实现是 uvicorn 的
`StreamingResponse`，逐 chunk 发；Go 侧若不显式 `http.Flusher`，小帧会被攒到连接结束才出现
（`curl -N`、OpenAI SDK 表现为「不流式」）。flush 只改到达时机，**不改任何字节**
（`ok-stream` / `sse-to-nonstream` 场景 flush 前后逐字节一致，已复核）。

## 二、响应形状（逐字节）

### 2.1 `/v1/chat/completions` 非流式（**紧凑 JSON，无空格**）

上游 `{"id":"msg_harness","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}` →

```json
{"id":"msg_harness","object":"chat.completion","created":1791052708,"model":"GLM-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}
```

- `id` = 上游 `id`；`created` = 当前 unix 秒；`model` = **上游 message 的 `model`**（缺键 ⇒ `null`）。
  ⚠️ 与流式路径口径**不同**：流式帧的 `model` 取**入站请求的 `model`** —— 首帧在解析任何事件
  **之前**就要写 model，且上游给 JSON 体时根本不解事件（见 §2.2）。样本里两者相同、不可区分，
  但两条路径的来源确实不同，实现要照实分开（`ok-openai` vs `chat-stream-from-json` 对照）。
- `choices[0].message.content` = **把所有 text 块拼起来**。
- `finish_reason`：`end_turn` → `"stop"`（**实测**）；`stop_sequence` → `"stop"`、
  `max_tokens` → `"length"`、`tool_use` → `"tool_calls"`、`stop_reason` 缺失/`null` ⇒ `null`、
  未知取值**原样透出**（这几条**未采样**，按两家公开规范对齐，登记在 §六）。
- `usage`：`prompt_tokens`←`input_tokens`、`completion_tokens`←`output_tokens`、`total_tokens` = 两者之和。
- **序列化用紧凑分隔符**（`,` / `:` 后无空格）—— 这是 Starlette `JSONResponse` 的形态，
  与出站请求体/SSE 帧的「默认分隔符（带空格）」**不同**。

### 2.2 `/v1/chat/completions` 流式（**带空格的 JSON**）

入站 `stream` 为**真值**时走这里。逐帧 `data: <json>\n\n`（**没有 `event:` 行**），
且 **JSON 是默认分隔符（`", "` / `": "`）**：

```
data: {"id": "chatcmpl-<24位hex>", "object": "chat.completion.chunk", "created": <ts>, "model": "<入站model>", "choices": [{"index": 0, "delta": {"role": "assistant", "content": ""}, "finish_reason": null}]}

data: {"id": "<上游 msg id>", "object": "chat.completion.chunk", "created": <ts>, "model": "<入站model>", "choices": [{"index": 0, "delta": {"content": "hello"}, "finish_reason": null}]}

data: {"id": "<上游 msg id>", "object": "chat.completion.chunk", "created": <ts>, "model": "<入站model>", "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4}}

data: [DONE]

```

- **首帧的 `id` 是新生成的 `chatcmpl-<24位hex>`**（不是上游 id）；**后续帧用上游 `id`**
  （即 `message_start.message.id`）。
- `created` 在整个流里是同一个值（首帧时取的 unix 秒）。
- `model` 取**入站请求的 model**（见 §2.1 的 ⚠️）。
- **末帧在收到 `message_delta` 时才发出**，不是循环结束后补 —— `delta` 空对象 +
  `finish_reason`（由 `delta.stop_reason` 映射，同 §2.1）+ `usage`。
- **`data: [DONE]\n\n` 是流的最后一帧**。
- 事件映射（逐条实测，`ok-openai-stream`）：
  `message_start` → 只**记** `message.id` 与 `message.usage.input_tokens`（不发帧）；
  `content_block_delta` → 内容帧（`delta.text` 为真值时；空串不发）；
  `message_delta` → 末帧（`usage.output_tokens` 作 `completion_tokens`）；
  `content_block_start` / `content_block_stop` / `message_stop` → **不发帧**。
  `total_tokens` = `prompt_tokens` + `completion_tokens`。
- **上游给 JSON 体（不是 SSE）时**：逐行扫不到 `data: ` 行 ⇒ **只发首帧 + `[DONE]`**，
  两帧、**不报错**（`chat-stream-from-json` 实测 len=240）。这也是「首帧 model 必取入站值」
  的证据 —— 那条路径根本没有上游 model 可读。

## 三、上游错误分类与状态机（**全部实测**）

### 3.0 `>>>` 是**请求进入路由时**打的，不是成功标记

```
>>> <reqid>  <model>  sync|stream  "<preview>"
```

- **每次请求都有一条**，与最终结果无关。实测：11 次请求 → 11 条 `>>>`，
  其中 10 次以 `503` 结束（池已被第一个请求抽干）。
- `<reqid>` = **16 位小写 hex**；同一个请求的所有日志行共用它。

后三个字段的取值规则**全部由探针钉死**（逐条证据见 `fixtures/route-log-probes.json`）：

| 字段 | 规则 | 最容易记错的地方 |
|---|---|---|
| `<model>` | `body.get("model") or "-"`：缺键 / `null` / **空串**都印 `-`；**不做模型名校验**，目录外模型原样回显 | 空串印 `-`（是真值判定，不是「有键就印」） |
| `sync\|stream` | 值的 **Python 真值**（`if body.get("stream")`） | **不是 `is True`**：`"false"` / `"0"` / `1` / `[0]` 都印 `stream`；`0` / `""` / `[]` / `{}` / `null` 都印 `sync` |
| `"<preview>"` | **最后一条 `role == "user"`** 消息的首段文本 | **不是首条消息**（m-multiturn 印的是末条 user 的 `again`）；role 比较**大小写敏感** |

`<preview>` 的细节（`[{user …}]` 逐个走）：

| `content` 形态 | 结果 |
|---|---|
| 字符串（**含空串**） | 取它 —— **空串也覆盖** |
| 数组 | 取第一个 `type == "text"` 块的 `text`；块里缺 `text` 键 ⇒ 取空串并覆盖；找不到 text 块 ⇒ **不覆盖** |
| 数字 / `null` / 对象 / 缺 `content` 键 | **不覆盖** |

所以 `[{user "A"}, {user ""}]` → `""`（被覆盖），而 `[{user []}, {user "B"}]` → `"B"`（没覆盖）。
「取不到就不覆盖」意味着预览值会**回落到更早的 user 消息**（`[{user "OLD"}, {user}]` → `"OLD"`）。

> 这条规则是在写 A4 时**由实测纠正**的：此前文档写「首条用户文本」，与 m-multiturn 的 `again` 不符。

### 3.0.1 `stream` 的真值语义同时决定 `/v1/chat/completions` 的响应形状

`/v1/messages` 的响应**完全不看** `stream`（上游给什么回什么）；
`/v1/chat/completions` 用同一个**真值**判定选择 JSON 还是 OpenAI SSE 输出
（探针 `c-strfalse__sse`：入站 `stream:"false"` 也走 SSE 转换）。见 §一 的表。

### 3.1 分类表

一次入站请求会**按顺序逐个账号尝试**，直到成功或池空。按上游响应分类：

| 上游 | 分类 | 日志文案 | 重试 | 账号后续状态 |
|---|---|---|---|---|
| **2xx** | 成功 | （无额外行；`>>>` 已在进入时打过） | — | 正常 |
| **4xx（除下四类）**：`400` / `404` / `408` / `422` … | **客户端错，直接透传** | `<!> <reqid> 上游错误 HTTP <code>（账号 X）` + `[~] <reqid> 上游 <code> 完整响应体: <body>` | 不重试、**不切账号** | 不变 |
| **401 / 403** | 鉴权失败 | `[~] <reqid> 账号 X 鉴权失败 <code>，切换下一个` | 不重试 | **标 invalid** |
| **402** | 额度用完 | `[~] <reqid> 账号 X 额度用完，切换下一个` | 不重试 | 切下一个 |
| **429** | 被限流 | `[~] <reqid> 账号 X 被限流 429，<N>s 后重试（k/5）` → `[~] <reqid> 账号 X 429 重试 5 次耗尽，切换下一个（账号保持可用）` | **重试 5 次**（连首次共出站 **6** 次），间隔 = `Retry-After` 秒；**缺该头时 60s** | **保持可用** |
| **5xx** | 上游故障 | `[~] <reqid> 账号 X 上游 HTTP <code>，5s 后重试（k/3）` → `[~] <reqid> 账号 X 上游 <code> 重试耗尽，冷却 300s，切换下一个` | **重试 3 次**（连首次共出站 **4** 次），间隔**固定 5s** | **冷却 300s** |
| 传输层错误 | 连接失败 | `[~] <reqid> 账号 X 连接失败，切换下一个` | 不重试 | 切换 |

**全部账号都失败** → `<!> <reqid>  无可用账号 / 额度均已耗尽 / 并发已满` →
入站 `503` + `{"error":{"message":"所有账号均不可用、额度已用完或并发已满，请在后台检查账号状态","type":"no_available_account"}}`。

### 3.2 这几行的依据（逐条实测，不猜）

| 实测项 | 结果 | 依据 |
|---|---|---|
| `400` / `404` / `408` / `422` | **都走「客户端错」透传分支**（不只是 400） | 探针 `p-404` / `p-408` / `p-422`：入站原样回 `404/408/422` + 上游错误体，且**只出站 1 次**（不切账号） |
| `402` | **独立分类**，文案 `额度用完，切换下一个` | 探针 `p-402`：`[~] … 账号 h-00 额度用完，切换下一个`，两账号各出站 1 次后 503 |
| `429` 带 `Retry-After: 7` / `: 2` | 间隔 = 7s / 2s | `behavior_diff.py --scenario upstream-429` / `upstream-429-retryafter` |
| `429` 的**次数** | 每个账号**出站 6 次**（1 首次 + 5 重试），日志 `（1/5）`…`（5/5）` | `behavior_diff.py --scenario upstream-429`：2 账号 × 6 = **12** 条出站记录 |
| `5xx` 的**次数** | 每个账号**出站 4 次**（1 首次 + 3 重试），日志 `（1/3）`…`（3/3）` | `behavior_diff.py --scenario upstream-500`：2 账号 × 4 = **8** 条出站记录 |
| `429` **不带** `Retry-After` | 间隔 **60s** | 探针 `p-429-noheader`：3 次间隔共 180s（实测总耗时 183.3s） |
| `5xx` 带 `Retry-After: 1` | **仍按 5s**（不读该头） | 探针 `p-500-ra1`：3 次重试实测 18.5s（≈3×5s + 启动开销） |
| `503` | 走 5xx 分支 | 探针 `p-503`：`上游 HTTP 503，5s 后重试（k/3）` → `冷却 300s` |
| `/v1/messages` + 上游 SSE + 入站**无** `stream` | **200 原样透传 SSE**（**不是** 502） | `behavior_diff.py --scenario sse-to-nonstream`（同名场景，已确认路径是 `/v1/messages`） |

> 3xx / 1xx 属**未覆盖**：分类链里没有它们的位置，Go 侧按「非 2xx 且未特别分类 ⇒ 走客户端错透传」
> 处理并在此登记（不是猜测出的「默认值」，而是把未覆盖分支的选择写明）。

> 注意：**401/403 的错误体不会到客户端**。客户端看到的是「所有账号都失败」后的 503。
> `observations.md` #19 记的「上游错误体原样透传」指的是**上游那一侧的响应**，
> 不是客户端拿到的入站响应 —— 只有「客户端错」这一类（与成功）才会到客户端。

## 四、账号「安装序」（eager + 后台）

安装序有**两级**，且都是**在账号加入时/进程启动时**发起，**不在转发请求的路径上**：

| 级别 | 时机 | 日志 |
|---|---|---|
| 进程级 | 进程启动 | `[+] install 安装初始化完成（configs=√，events=app_launch,app_daily_active）` |
| 账号级 | 账号加入池时（含启动时载入既有账号） | 成功 `[+] install 账号 X 安装序完成（install_id=<uuid>）`；降级 `[~] install 账号 X 安装序部分失败: <原因>` |

每一步都是 1 次 + 2 次出站：

1. `GET https://zcode.z.ai/api/v1/client/configs?app_version=3.14.4`（不带鉴权）
2. `POST https://zcode.z.ai/api/v1/event/report` × 2（`element_name` = `app_launch`、`app_daily_active`，带账号指纹）

**依据（一次 41 账号的全量采样）**：

- 出站计数：`client/configs` **42** 条、`event/report` **84** 条、`api.z.ai/v1/messages` **41** 条。
  42 = 41 个账号 + 1 次进程级；84 = 42 × 2 ⇒ **每个安装序恰好 1 configs + 2 events**。
- 日志计数：`安装序完成` 33 + `安装序部分失败` 8 = **41**（= 账号数），`安装初始化完成` 1。
- **顺序证据**：全部 41 条安装序日志出现在**首个转发请求之前或与之交错**，
  且 `[+] install 账号 h-00 安装序完成` 出现在 `429 重试（1/5）` 与 `（2/5）` 之间 ——
  即请求在 7s 退避里睡着时安装序才跑完 ⇒ **安装序是并发后台任务**，不阻塞转发。

> **修正 `observations.md` #36**：那里记的「每次尝试前都会拉一次 `client/configs`」是
> 把「账号级安装序」误当成了「每次转发前的探活」。计数（42 vs 41）与交错顺序都指向
> **安装序按账号跑一次**，转发路径上**没有任何 configs 调用**。

> 属 A5（`internal/install`）。A4 的转发路径**不调用** configs/event。

## 五、模型表：**编译期常量**（观察 #26 的待钉死项已钉死）

把 `client/configs` 阻断成 502 后，`GET /v1/models` **仍返回**：

```json
{"object":"list","data":[{"id":"GLM-5.3-Flash","type":"model","display_name":"GLM-5.3-Flash","created_at":"2025-01-01T00:00:00Z"},{"id":"GLM-5.3","type":"model","display_name":"GLM-5.3","created_at":"2025-01-01T00:00:00Z"}]}
```

- 顺序是 **Flash 在前**，而 `client/configs` 的 `builtinModels` 是 **`GLM-5.3` 在前** ⇒
  **不是从 `builtinModels` 派生的**，是编译期常量（A3 的原始判断成立）。
- 因此 Go 实现**不需要**「从 configs 派生模型表」这条路径，也不需要为 configs 失败做回落 ——
  它本来就是常量。

## 六、本文件未覆盖

- **真实上游的 200 回执**：这里的 200 是**按公开 Anthropic Messages API 规范合成的**，
  不是采样来的。真实回执的字段集可能更宽（如 `container` / `service_tier`）——
  但 `/v1/messages` 是字节透传，所以宽度不影响保真；只影响 `/v1/chat/completions` 的转换完整性。
- 风控 `3012` / 验证码挑战的真实回执（属 A6）。
- `content` 里 `tool_use` / `tool_result` / `image` 块的转换。
- 并发槽（`account_concurrency`）耗尽时的排队行为。
- **3xx / 1xx 上游状态码**：分类链里没有它们的位置。Go 侧按「非 2xx 且未特别分类
  ⇒ 走客户端错透传」处理（见 3.2 末注），并把该选择登记在此，而不是当成已知默认值。
- **`Retry-After` 不是整数**（如 HTTP-date 形态）时的行为。
- **`/v1/chat/completions` 的入站字段映射**：`role: system` 怎么处理、`tools` / `temperature` /
  `stop` 等 OpenAI 字段是否搬运 —— **均未采样**（清单见 `fixtures/chat-completions-requests.json`
  的 `_gaps`）。A4 的落地口径见 §六之三。
- **上游没有 `Content-Type` 响应头时**的入站 `Content-Type`（假上游总会带一个，测不出）。

## 六之二、**有意偏离**（参考实现是缺陷，本实现不照抄）

以下两处参考实现的行为是**未处理异常**（入站 `500 text/plain Internal Server Error`、
连 `>>>` 日志行都没打、**零出站**）。照抄一个崩溃不是「保真」，本实现改为**明确拒绝并说明原因**
（`400` + `{"error":{"message":…,"type":"invalid_request_error"}}`），在此登记：

| 入站体 | 参考实现 | 本实现（有意偏离） | 证据 |
|---|---|---|---|
| `model` 是**非字符串、非 `null`**（如 `123`） | 500 崩溃 | `400 model 必须是字符串`（`bodytransform.ErrModelNotString`） | 探针 `p-model-number` |
| `messages[].content[]` 的 text 块里 `text` **不是字符串**（如 `123`） | 500 崩溃 | 该条预览取空串，**正常转发**（不崩） | 探针 `p-text-nonstring` |

> 判据的取舍：这两处的「参考行为」不构成任何可依赖的契约（客户端只会拿到 500 与
> 一个空白页面），而项目铁律是「未实现的协议一律明确报错并附原因，绝不回落到近似协议试一下」。
> 因此选择拒绝/降级，而不是复刻崩溃。**除此之外**的转发行为一律逐项对齐。

> **本轮已由实测补齐、不再是未覆盖**：`>>>` 行第 3 字段在 `stream:true` 时印 `stream`
> （`ok-stream` / `ok-openai-stream` 两个场景的日志）；非对象根 → 400
> `请求体必须是 JSON 对象`（两个入口一致，样本 `gateway/26-messages-notobject.POST.json`）；
> `messages` 缺失 / 不是数组、`content` 是数字 / `null` 时**原样转发不改写**（形状探针）；
> `>>>` 行第 4 字段是**最后一条 `role=="user"`** 的首段文本（§3.0，此前记成「首条」是错的）；
> `/v1/messages` 的响应头只搬上游 `Content-Type`（§一，探针 `h-*`）。
> **`/v1/chat/completions` 的响应转换与 SSE 转换本轮也全部实测**：非流式紧凑 JSON（含
> 键序）、流式四帧序列（首帧新 id、末帧 `usage`、`data: [DONE]`）、上游给 JSON 体时只发
> 首帧 + `[DONE]`、入站 `stream` 假值 + 上游 SSE/非 JSON ⇒ 502 —— 见 §2.1/§2.2 与
> `internal/compat/chat_test.go`、`internal/gateway/chat_test.go`。

## 六之三、`/v1/chat/completions` 重建的落地口径（A4，**未采样**处的选择）

出站重建是**白名单**：只写 `model, messages, max_tokens`（+ 真值 `stream` 追加末尾），
其余入站字段（`temperature` / `top_p` / `tools` / `n` / `system` …）**一律丢弃**。
这是由「重建」语义 + 三条实测配对推出的确定行为，不是猜测。以下**具体边界**未采样，
列出本实现的选择（都可被将来的一条新探针推翻）：

| 边界 | 本实现的选择 | 依据 / 备注 |
|---|---|---|
| `model` / `messages` / `max_tokens` **缺失** | 写 `null`（保留键位） | Python `body.get(key)` → `None` 的形态；三条实测配对里三个键都在，缺键未采样 |
| `messages[].content` 是字符串 | 包成 `[{"type":"text","text":…}]` | **已实测**（与 `/v1/messages` 同一规则，`c-string`/`ok-openai` 配对） |
| `stop_reason` 非 `end_turn` | `stop_sequence`→`stop`、`max_tokens`→`length`、`tool_use`→`tool_calls`、缺失/`null`⇒`null`、未知**原样透出** | 只有 `end_turn→stop` 实测；其余按两家公开规范对齐 |
| token 数是**非整数** | 截断取整（`3.0` → `3`） | 实测都是整数；非整数未采样 |
| 上游 `data: ` 行是**非法 JSON** | **跳过**该行（不中断、不报错） | 假上游总给合法 JSON，未采样 |
| 上游 `content` 本身是**字符串** | 直接当文本用 | 未采样（规范里 `content` 是块数组） |
| 上游 200 但根**不是对象**（如 `[1,2]`） | 502 `上游响应格式异常` | 参考实现在此会未处理异常（`.get` on list）；本实现按铁律明确报错 |

> 这条与 §六之二的区别：§六之二是「参考实现会**崩溃**」的分支（本实现改为明确拒绝）；
> 这里是「参考实现**能跑**、只是没采到样本」的分支（本实现按最接近的已知语义实现，
> 并把选择登记在此，不冒充实测）。

