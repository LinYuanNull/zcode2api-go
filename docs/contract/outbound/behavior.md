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
| `POST /v1/chat/completions` | **重建**：键序固定 `model, messages, max_tokens`（+ `stream` 若入站有） | **必须解析**：转成 OpenAI 形状（JSON）或 OpenAI SSE；解析失败 → 502 |

**`/v1/messages` 不解析响应**，所以上游给什么就回什么：

| 上游响应 | 入站 |
|---|---|
| 200 + JSON | 200 + **同样的字节**，`Content-Type: application/json` |
| 200 + SSE（非流式请求） | 200 + **同样的字节**，`Content-Type: text/event-stream; charset=utf-8` |
| 200 + 非 JSON 文本（`not json at all`） | 200 + `not json at all` |
| 200 + 错误体 `{"error":…}` | 200 + 同样的字节 |
| 400 | 400 + 上游错误体逐字透传 |

**`/v1/chat/completions` 必须解析**：

| 上游响应 | 入站 |
|---|---|
| 200 + Anthropic JSON | 200 + 转换后的 OpenAI JSON |
| 200 + SSE | 200 + 转换后的 OpenAI SSE |
| 200 + SSE（但入站**非**流式） | **502** `{"error":{"message":"上游响应格式异常","type":"upstream_error"}}` |
| 200 + 非 JSON 文本 | 同上 502 |

## 二、响应形状（逐字节）

### 2.1 `/v1/chat/completions` 非流式（**紧凑 JSON，无空格**）

上游 `{"id":"msg_harness","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}` →

```json
{"id":"msg_harness","object":"chat.completion","created":1791052708,"model":"GLM-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}
```

- `id` = 上游 `id`；`created` = 当前 unix 秒；`model` = 上游 `model`。
- `choices[0].message.content` = **把所有 text 块拼起来**。
- `finish_reason`：`end_turn` → `"stop"`。
- `usage`：`prompt_tokens`←`input_tokens`、`completion_tokens`←`output_tokens`、`total_tokens` = 两者之和。
- **序列化用紧凑分隔符**（`,` / `:` 后无空格）。

### 2.2 `/v1/chat/completions` 流式（**带空格的 JSON**）

逐帧 `data: <json>\n\n`（**没有 `event:` 行**），且 **JSON 是默认分隔符（`", "` / `": "`）**：

```
data: {"id": "chatcmpl-<16位hex>", "object": "chat.completion.chunk", "created": <ts>, "model": "<model>", "choices": [{"index": 0, "delta": {"role": "assistant", "content": ""}, "finish_reason": null}]}

data: {"id": "<上游 msg id>", "object": "chat.completion.chunk", "created": <ts>, "model": "<model>", "choices": [{"index": 0, "delta": {"content": "hello"}, "finish_reason": null}]}

data: {"id": "<上游 msg id>", …, "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": {"prompt_tokens":…,"completion_tokens":…,"total_tokens":…}}
```

- **首帧的 `id` 是新生成的 `chatcmpl-<16位hex>`**（不是上游 id）；**后续帧用上游 `id`**。
- `created` 在整个流里是同一个值（首帧时取的 unix 秒）。
- 末帧 `delta` 为空对象 + `finish_reason` + `usage`。

## 三、上游错误分类与状态机（**全部实测**）

一次入站请求会**按顺序逐个账号尝试**，直到成功或池空。按上游响应分类：

| 上游 | 分类 | 日志文案 | 重试 | 账号后续状态 |
|---|---|---|---|---|
| 2xx | 成功 | `>>> <reqid>  <model>  sync\|stream  "<首条文本>"` | — | 正常 |
| **400** | **客户端错，直接透传** | `<!> 上游错误 HTTP 400（账号 X）` + `[~] 上游 400 完整响应体: <body>` | 不重试、**不切账号** | 不变 |
| **401 / 403** | 鉴权失败 | `[~] 账号 X 鉴权失败 <code>，切换下一个` | 不重试 | **标 invalid** |
| **429** | 被限流 | `[~] 账号 X 被限流 429，<N>s 后重试（k/5）` → `[~] 账号 X 429 重试 5 次耗尽，切换下一个（账号保持可用）` | **5 次**，间隔 = `Retry-After` 秒（实测 `7`→7s、`2`→2s） | **保持可用** |
| **5xx** | 上游故障 | `[~] 账号 X 上游 HTTP 500，5s 后重试（k/3）` → `[~] 账号 X 上游 500 重试耗尽，冷却 300s，切换下一个` | **3 次**，间隔固定 **5s** | **冷却 300s** |
| 传输层错误 | 连接失败 | `[~] 账号 X 连接失败，切换下一个` | 不重试 | 切换 |

**全部账号都失败** → `<!> 无可用账号 / 额度均已耗尽 / 并发已满` →
入站 `503` + `{"error":{"message":"所有账号均不可用、额度已用完或并发已满，请在后台检查账号状态","type":"no_available_account"}}`。

> 注意：**401 的错误体不会到客户端**。客户端看到的是「所有账号都失败」后的 503。
> `observations.md` #19 记的「上游错误体原样透传」指的是**上游那一侧的响应**，
> 不是客户端拿到的入站响应 —— 只有 400（与成功）才会到客户端。

## 四、账号「安装序」（eager）

每个账号在被使用前要跑一遍安装序，**在池内预先铺开**：

1. `GET https://zcode.z.ai/api/v1/client/configs?app_version=3.14.4`（不带鉴权）
2. `POST https://zcode.z.ai/api/v1/event/report` × 2（`app_launch`、`app_daily_active`，带账号指纹）

- 成功：`[+] install 账号 X 安装序完成（install_id=<uuid>）`
- 降级：`[~] install 账号 X 安装序部分失败: <原因>`（例如 `client/configs 失败: …`）
- 进程启动时：`[+] install 安装初始化完成（configs=√，events=app_launch,app_daily_active）`

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
