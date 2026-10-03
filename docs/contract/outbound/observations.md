# 出站契约观察（A4 采样）

> 采集方式与工具见 [`README.md`](README.md)。这里只记**实测钉死的规则**与**未覆盖项**。
> 纪律同 A1/A2：**未覆盖的分支不凭猜测补全**，实现时显式报错。

## 一、上游主机与端点

| # | 事实 | 依据 |
|---|---|---|
| 1 | 上游主机只有两个：**`zcode.z.ai`**（配置 / 遥测）与 **`api.z.ai`**（消息转发） | MITM 捕获的 CONNECT 目标（`--log-connects`） |
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

## 三、入站 → 出站的字段映射（**全场景实测**）

全场景采样（11 个场景，41 条真实转发请求）后，变换规则收敛得非常干净：

| # | 事实 | 依据 |
|---|---|---|
| 9 | **唯一被改写的字段是 `messages[].content`**：值为**字符串**时包装成 `[{"type":"text","text":<原串>}]`；值已是**数组**时**原样** | `m-string` vs `m-array` 的出站 body 逐字相同 |
| 10 | `model` / `max_tokens` **原样**透传 | 全部场景 |
| 11 | `system`（字符串）**原样**透传，**不**包装成数组 | `m-system` 出站 body |
| 12 | `system`（数组）**原样**透传 | `m-system-arr` 出站 body |
| 13 | 多轮 `messages`（含 `assistant` 角色）**逐个**按 #9 处理 | `m-multiturn` 出站 body |
| 14 | `tools` **原样**透传 | `m-tools` 出站 body |
| 15 | `stream: true` **原样**透传 | `m-stream` 出站 body |
| 16 | `temperature` / `top_p` / `stop_sequences` / `metadata` **原样**透传 | `m-extra` 出站 body |
| 17 | 出站鉴权头是 **`X-Api-Key`**（不是 `Authorization`），值为账号 token **明文** | 全部场景 |
| 18 | 固定头：`Anthropic-Version: 2023-06-01`、`User-Agent: ZCode/3.14.4`、`X-Zcode-Agent: glm`、`X-Zcode-App-Version: 3.14.4`、`Http-Referer: https://zcode.z.ai` | 全部场景 |
| 19 | 上游错误体**原样透传**：`{"error":{"message":"token expired or incorrect","type":"401"}}`，`type` 是**状态码字符串** | `02-messages.POST.json` |
| 20 | `client/configs` **不带任何鉴权头**（公开目录） | `01-client-configs.GET.json` |
| 21 | `client/configs` 响应 **gzip 压缩**，出站客户端必须解压 | 捕获记录的 `Content-Encoding: gzip` |
| 22 | 遥测 `event/report` 的 `User-Agent` 是 `python-httpx/0.28.1`（与消息转发不同） | `03-event-report.POST.json` |
| 23 | 遥测 body 的 `client_timezone` / `client_language` / `screen_resolution` / `device_os_category` / `device_os_version` / `device_mid` **全部取自账号指纹** | 同一次运行内两次上报用了两个不同账号的指纹 |

### 三之二、**两个入口的出站 body 键序不同**（A4-4 的硬判据）

| # | 事实 | 依据 |
|---|---|---|
| 24 | `/v1/messages` 的出站 body **保持入站键序**（把 `messages` 放在入站时它所在的位置） | `m-extra` 入站/出站键序一致 |
| 25 | `/v1/chat/completions` 的出站 body 是**重建**的，键序固定为 **`model`, `messages`, `max_tokens`** | `c-string` 入站 `model,max_tokens,messages` → 出站 `model,messages,max_tokens` |

> #24 说明 `messages` 入口是「读原文 → 只改 `content` → 写回」的**保序**实现（与 A2 的
> `sanitizeOrdered` 同一思路）；#25 说明 `chat/completions` 入口是「解析成结构 → 重新编码」，
> 于是键序由**字段声明序**决定。两者不可互推，Go 实现要分别对齐。

## 四、模型表：`/v1/models` 的来源（**修正 A3 的一处假设**）

| # | 事实 | 依据 |
|---|---|---|
| 26 | 入站 `GET /v1/models` 返回 `GLM-5.3` / `GLM-5.3-Flash`，**恰好等于** `client/configs` 的 `data.builtinModels[].modelId` | `01-client-configs.GET.json` + `gateway/23-models-ok.GET.json` |
| 27 | **但顺序不同**：`/v1/models` 是 Flash 在前，`builtinModels` 是 `GLM-5.3` 在前 ⇒ **不是从 configs 派生的** | 两处样本对照 |
| 28 | **模型名不影响「是否出站」**：目录外模型（`no-such-model-xyz`）与目录内模型（`GLM-5.3`）**都会**尝试转发 | 全场景采样：`m-unknown-model` 同样产出转发请求 |
| 29 | 把 `client/configs` 阻断成 502 后 `/v1/models` **仍返回同样两张表** ⇒ **模型表是编译期常量**，无需回落逻辑 | `behavior.md` 第五节（`tools/behavior_diff.py --scenario configs-down`） |

> **待钉死项已钉死**（原记：「把 `client/configs` 阻断后是否回落到内置常量」）：
> 实测**是常量**。A1 的「`app/constants.AVAILABLE_MODELS`」判断成立，
> Go 实现不需要「从 configs 派生模型表」这条路径。

## 五、调度行为（**本轮修正了上一轮的错误结论**）

| # | 事实 | 依据 |
|---|---|---|
| 29 | 新增的 apiKey 账号初始 `status=active` ⇒ `quota_pool` 计入 ⇒ **会真的发起出站转发** | `02-accounts-one.GET.json` + 实测池计数 0→1 |
| 30 | **每个账号有一套「安装序」**：拉一次 `client/configs`，再上报 `app_launch` 与 `app_daily_active` 两个事件；成功则打印 `install_id` | 日志 `[+] install 安装初始化完成（configs=√，events=app_launch,app_daily_active）` / `[+] install 账号 X 安装序完成（install_id=…）` |
| 31 | 安装序**在池内预先铺开**（eager）：一次采样里 40 个账号全部跑完安装序，与请求数无关 | 日志里 33 条 `安装序完成` 出现在首个请求之前/之间 |
| 32 | **一次请求会「逐个账号」尝试到成功或池空**：失败就把该账号标记后切下一个，全部失败才 503 | 日志 `[~] … 账号 X 鉴权失败 401，切换下一个` ×N → `<!> 无可用账号 / 额度均已耗尽 / 并发已满` |
| 33 | 上一轮记的「≈4–5 个账号/请求」**是池被部分消耗后的假象**。真实语义是「**按顺序试到成功为止**」，假 token 必然全败 ⇒ 一个请求就能把整个池抽干 | 池 41 → 首个请求后 `pool=0` |
| 34 | 失败分四类，日志文案固定：`鉴权失败 401`（上游 401）、`连接失败`（传输层）、`install 账号 X 安装序部分失败: <原因>`（安装序降级，如 `client/configs 失败`）、终态 `无可用账号 / 额度均已耗尽 / 并发已满` | 全场景日志 |
| 35 | 账号首次转发失败（401）后转 `invalid`，后续请求不再用它 | 池计数下降 + 账号 `recent_results` |
| 36 | **每次尝试前都会拉一次 `client/configs`**（单次请求内可看到多次 configs 抓取） | 42 条 configs vs 41 条转发 |

> **结论**：上一轮登记的「为什么部分请求完全没有 `api.z.ai` 出站」**已解释** ——
> 池被前一个请求抽干后，后续请求**在本地直接 503，零出站**。与模型名、代理、
> 安装序都无关。**不是未实现的分支**，不需要在 Go 里显式报错，而要在调度器里正确实现
> 「逐个账号试到成功」的语义。

## 六、工具缺陷（导致上一轮漏采，已修）

| # | 缺陷 | 症状 | 修法 |
|---|---|---|---|
| 37 | MITM 的 `stderr` 给的是**没人读的 PIPE** | 管道写满（约 64 KB）后代理卡死在 `write` 上，后面的请求**一条记录都不留** | 落文件，不用 PIPE |
| 38 | 叶子证书只预生成 `--hosts` 里的 SAN | 连到名单外主机时 TLS 必然失败，且失败发生在握手阶段 ⇒ **同样不留记录**，分不清「没出站」与「出站到了没覆盖的主机」 | 按 CONNECT 目标**动态签发**叶子证书 + 握手失败也落一条 `kind=connect` 记录 |
| 39 | 靶机路由日志带 **ANSI 色码** | 按 `>>>` 前缀过滤时全部匹配不上，看起来「靶机什么都没说」 | 先剥 `\x1b\[[0-9;]*m` 再匹配 |

## 七、未覆盖（**不凭猜测补全**）

- **200 成功响应体**与 **SSE 分块**（`stream:true` 的真实回执）—— 需真实账号。
- 上游 **429 / 5xx / 风控 3012 / 验证码挑战** 的真实回执与冷却时长。
- `client/configs` 的 `builtin_provider_config_json`（指向 CDN 的二次配置）内容。
- `messages[].content` 里**非 text 块**（`image` / `tool_use` / `tool_result`）的变换。
- `/v1/chat/completions` 在 `stream:true` 下的出站 body（该场景池已空，未采到）。
