# 管理侧出站契约观察（A5 采样）

> 口径：**全部结论来自「本机运行靶机 + 本地 MITM」的真实流量**，采样器为
> `tools/capture_admin_outbound.py`。这里只写实测到的东西；没采到的分支一律进
> [第九节](#九未覆盖不凭猜测补全)，**不凭猜测补全**。
>
> 与 A4（`../outbound/`）的关系：A4 采的是**网关入站 → 上游**的转发链路，A5 采的是
> **网关管理面 → 上游**的登录 / 额度 / 领取链路。两者出站头有真实差异（见第七节）。

## 一、上游主机与端点

| route | 方法 | 用途 | 谁触发 |
|---|---|---|---|
| `https://zcode.z.ai/api/v1/oauth/cli/init` | POST | 设备码登录：开一个 flow | `POST /admin/api/login/start` |
| `https://zcode.z.ai/api/v1/oauth/cli/poll/<flow_id>` | GET | 轮询 flow 状态 | `GET /admin/api/login/poll/<flow_id>` |
| `https://zcode.z.ai/api/v1/zcode-plan/usage` | GET | 额度（用量）查询 | `active` JWT 账号的 **新增 / 启动 / refresh** |
| `https://zcode.z.ai/api/v1/zcode-plan/billing/current` | GET | 当前账期 | 同上（与 usage 同批并发） |
| `https://zcode.z.ai/api/v1/zcode-plan/billing/balance` | GET | 余额 | 同上（与 usage 同批并发） |
| `https://zcode.z.ai/api/v1/client/configs?app_version=<ver>` | GET | 阿里验证码 SDK 初始化参数 | `GET /admin/api/claim/captcha-config`（**A6 采样**；A4 已采过同一端点的完整响应，样本 `../outbound/01-client-configs.GET.json`） |

出站地址**硬编码**（A4 已实测 10 个候选环境变量都不生效），与 A4 的路由同一个 host。

## 二、管理侧出站**同样走代理**（更正旧记）

旧记曾写「billing / 验证码走**强制直连**」。本轮 MITM 实测**否定**了这条：
管理侧出站同样读 `HTTPS_PROXY`，且 `httpx` 在 `trust_env=True` 下读 `SSL_CERT_FILE` ⇒
把 `HTTPS_PROXY` 与 `SSL_CERT_FILE` 指向本地 MITM 即可解密**全部**管理侧出站（含 billing）。
这正是本目录能采到样本的原因。

> 推论给实现：出站 client **必须**保留「读环境代理 + 信任 `SSL_CERT_FILE`」的语义，
> 否则在企业代理环境里行为与靶机不一致。

## 三、OAuth 设备码链路

### 3.1 请求头**极简**（与额度查询形成对比）

```
Accept: */*
Accept-Encoding: gzip, deflate
Authorization: Bearer <64 位小写 hex>
Connection: keep-alive
Content-Type: application/json
User-Agent: python-httpx/0.28.1
```
（POST 另有 `Content-Length`；**没有**任何 `X-Client-*` / `X-Device-Mid` 指纹头。）

### 3.2 Bearer 令牌的来源与复用（实测钉死）

- 该 64 位小写 hex 由**进程内随机生成、不落盘**。
- 它**同时**是 init 响应回传的 `poll_token` —— 实测 `init` 请求头里的 Bearer
  与响应 `data.poll_token` 的 SHA-256 **一致**。
- 后续 `poll` 请求复用**同一个** token（`poll` 的请求 Bearer == `init` 的 Bearer）。

> 形状是契约（64 位小写 hex）；取值不是契约（每次运行都变）。
> 实现只需保证「进程内生成一次、init 与所有 poll 复用同一值、不落盘」。

### 3.3 `oauth/cli/init` 的响应形状

```json
{"code":0,"msg":"","data":{
  "flow_id": "<flow-id>",
  "poll_token": "<hex64>",
  "authorize_url": "https://chat.z.ai/api/oauth/authorize?client_id=client_P8X5CMWmlaRO9gyO-KSqtg&redirect_uri=https://zcode.z.ai/api/v1/oauth/cli/callback/zai&state=<hex>&response_type=code",
  "expires_at": 1791063713,
  "poll_interval_sec": 2
},"logid": "<hex>"}
```

- 请求体固定 `{"provider":"zai"}`（18 字节）。
- **响应是 gzip**（`Content-Encoding: gzip`），采样器落盘的是解压后明文。
- 注意 `data` 内层某些键后**带空格**（`"flow_id": "..."`）—— 这是上游自己的序列化
  形态，保留原样即可，不构成我方需要复刻的契约（我方只做透传）。

### 3.4 管理侧 `login/start` 对 init 的**变换**（关键）

靶机**不直接透传** init 响应，而是重组成：

```json
{"flow_id":"<redacted:value>","authorize_url":"https://chat.z.ai/api/oauth/authorize?...","expires_in":300}
```

对照结论：

| 上游 init 字段 | 管理侧 `login/start` | 说明 |
|---|---|---|
| `data.flow_id` | `flow_id` | 原样 |
| `data.authorize_url` | `authorize_url` | 原样（含 `client_id` / `redirect_uri` / `state`） |
| `data.expires_at`（unix 秒） | `expires_in: 300` | **换算为固定 300 秒 TTL** |
| `data.poll_token` | **丢弃** | 不回传给管理前端（留在网关内部） |
| `data.poll_interval_sec` | **丢弃** | 网关内部用（见 3.5） |
| `logid` | **丢弃** | |

> 我方 Go 实现必须复刻这个**变换**（`expires_in` 固定 300、丢掉 `poll_token`/`logid`），
> 而不是把上游 `data` 整体返回。

### 3.5 `oauth/cli/poll/<flow_id>` 与「逐次出站」

- 请求：`GET .../oauth/cli/poll/<flow_id>`，请求头与 3.1 同（同一 Bearer）。
- 响应：`{"code":0,"msg":"","data":{"status":"pending"},"logid":"<hex>"}`。
- 实测 `data.status` 至少见 **`pending`**（另有 `ready` / `failed` / `expired` 属未覆盖，见第九节）。
- **没有本地时间门控**：管理侧**每次** `login/poll` 都**逐次**打一次上游 poll ——
  实测 1.0s 间隔连打 6 次 → **6 次出站**、全部 `200`；2.2s 间隔同样 **6/6**。
  `poll_interval_sec`（本样本 2）**不是**出站门控：它随 `login/start` 一起被丢弃。
- **上游偶发 429（非确定性）**：0.4s 间隔快打会**间歇性**触发上游限流 ——
  实测一次拿到 `[200,200,200,429]`，**同参数另一次却是 6 个 `200`**。
- **出站失败后回退缓存**：一旦某次 poll 出站失败（如上例的 429），
  **其后该 flow 的 poll 不再打上游**，管理侧仍返 `pending`
  （实测 6 次 0.4s 间隔里第 5/6 次**零出站**）。
- **未知 flow 不出站**：`GET /admin/api/login/poll/unknown-flow` 管理侧直接回
  `{"status":"expired"}`，MITM 侧**零出站** ⇒ 这是网关**本地**状态机给出的结论。

### 3.6 管理侧 `flow_id` **就是**上游 `flow_id`（实测）

同一 flow 三个值 **SHA-256 全等**：`login/start` 返回的 `flow_id` == 上游 init 响应的
`data.flow_id` == 上游 poll 请求路径里的 id（**32 位 hex**）。

> 即：管理侧**沿用**上游的 `flow_id`，**不是**本地另生成再映射。
> 给实现：`login/poll/<id>` 的 `id` 必须能直接用作上游 poll 的路径参数。

## 四、额度查询（`zcode-plan/*`）

### 4.1 三条并发 + **共用同一个 `X-Request-Id`**

一次额度查询会**在同一毫秒内并发**发出三条（实测时间戳差 ≤ 1ms）：

```
GET /api/v1/zcode-plan/usage
GET /api/v1/zcode-plan/billing/current
GET /api/v1/zcode-plan/billing/balance
```

三条**共用同一个 `X-Request-Id`**（UUID 形态，见 4.2），而**不同批**（不同触发点）
之间 `X-Request-Id` **不同**。

> `X-Request-Id` 是**客户端逐批生成**的 UUID（不是服务端追踪号）：它是契约的一部分，
> 所以夹具里**请求侧的 `X-Request-Id` 未脱敏**（响应侧那个才脱敏，见第八节）。

### 4.2 请求头**全套指纹**

额度查询带完整客户端指纹头（与 OAuth 的极简头形成对比）：

```
Accept, Accept-Encoding, Authorization: Bearer <JWT>,
Connection, Content-Type, Http-Referer: https://zcode.z.ai,
User-Agent: ZCode/3.14.4,
X-Client-Language, X-Client-Timezone, X-Device-Mid,
X-Os-Category, X-Os-Version, X-Platform, X-Release-Channel,
X-Request-Id, X-Title: Z Code@electron, X-Zcode-App-Version: 3.14.4
```

- `Authorization` 用的是**账号自己的 JWT**。
- 指纹取值落在 `../store/fingerprint-shape.json` 的取值池里（`language` / `timezone` /
  `platform` / `arch` / `os_version` / `screen`），**每个安装随机**（本样本是
  `de-DE` / `Europe/Berlin` / `darwin` / `arm64` / `23.6.0`）。
- **平台映射（12 次抽样实测，8 种组合）**：
  - `X-Os-Category` = `macos`（`platform=darwin`）/ **`windows`**（`platform=win32`）；
  - `X-Platform` = `<platform>-<arch>`（`darwin-arm64` / `darwin-x64` / `win32-x64`）；
  - `X-Os-Version` 与 `platform` 严格同域（darwin → `2x.y.z`、win32 → `10.0.2xxxx`），
    与 `../store/fingerprint-shape.json` 的池一致 —— **不存在交叉**。
- `X-Title` 值必须逐字是 `Z Code@electron`（含 `@`）；`X-Release-Channel` 实测恒为 `stable`。
- `Content-Type: application/json` 出现在 **GET** 请求上 —— 实测如此（照发，别「优化」掉）。

### 4.3 响应码（凭据无效时）

| 端点 | 状态 | 响应体 |
|---|---|---|
| `zcode-plan/usage` | **404** | `404 page not found`（`text/plain`，gzip） |
| `zcode-plan/billing/current` | **401** | 空（`Content-Length: 0`） |
| `zcode-plan/billing/balance` | **401** | 空（`Content-Length: 0`） |

> 三者**不一致**（usage 走 404 而非 401）—— 这是上游的真实行为，实现时按端点分别判定「凭据失效」，
> 不能只认 401。

### 4.4 触发条件（本轮核心结论，实测钉死）

对 **`status=active`** 的 JWT 账号才查上游额度。触发点有三：

1. **`POST /admin/api/accounts`（新增账号）—— 主触发点。** token 形状是 JWT ⇒ 新增时立刻校验
   （`probe_a5_attr*.py` 归因证明：add 之后立刻出现 3 条额度出站）。
2. **启动**：对库里的 `active` 账号自刷一次。
3. **`POST /admin/api/accounts/<id>/refresh`**：对 `active` 账号会真查。

账号一旦被判 **`invalid`（凭证失效）则永不再查**：`refresh` 直接回**缓存响应**、**零出站**，
需重新授权才会再查。

> `quota_refresh_interval` 是**限流用的时间窗**（默认 **1800** 秒，见
> `../admin/17-settings-get.GET.json` 的设置项）。置 **0** 时 `active` 账号的 `refresh`
> 可**立即**重查（采样器的段 3 就是靠它采到「新鲜」分支）；该值在**进程启动时快照**。

### 4.5 `refresh` 的**两种响应形态**（对照判据）

| 形态 | 触发场景 | 形状 |
|---|---|---|
| **FRESH**（真的查了上游） | `active` 账号且未命中时间窗 | `{"ok":false,"result":{"error":"凭证失效，请重新授权"},"account":{…}}` |
| **CACHED**（未出站） | 账号已 `invalid`，或命中 `quota_refresh_interval` 窗 | `{"ok":false,"message":"凭证失效，请重新授权","account":{…}}` |

> **判据是键名**：真查上游走 `result`（嵌套对象）；命中缓存走 `message`（顶层字符串）。
> 两者的 `account.status` 都是 `invalid`，**不能只看状态判别是否出站**。

## 五、claim 链路：三条**不出站**，`captcha-config` **要出站**

实测 `claim/preview`、`claim`、`claim/manual` **三条全部零出站**（只读已存状态）。
`claim/manual` 的请求体形状为 `{"account_id":"<id>","captcha_verify_param":"<str>"}`。

⚠️ **A6 更正**：`GET /admin/api/claim/captcha-config` **不是零出站**。
A5 曾记「同样零出站，返回本地配置」——那是**未经采样的推断**（A5 的 MITM 只覆盖了三条
claim 路由），本轮查上游实现后否定：该路由每次都经 `captcha_manager.fetch_config()`
去拉 `GET {origin}/api/v1/client/configs?app_version=<ver>`（即上表第 6 条），
带 600s 缓存；**拉失败**时才回落静态默认值。响应体：

```json
{"enabled":true,"scene_id":"11xygtvd","region":"cn","prefix":"no8xfe"}
```

四个字段就是公开目录里 `data.configs.captcha` 的那四项（`sceneId` 改名 `scene_id`，
丢掉 `skip_model_request`），而默认值与它**同值** —— 所以「拉到」与「拉不到」在当前
环境里产出同一个响应体。上游默认值常量是
`CAPTCHA_DEFAULTS = {"enabled": True, "prefix": "no8xfe", "region": "cn", "sceneId": "11xygtvd"}`。

它们对「凭证失效」账号的响应形状（入站旁录，见 `fixtures/admin-responses.json`）：

```json
// claim/preview
{"preview":[{"account_id":"…","account_name":"…","plans":[],"error":"凭证失效，请重新授权",
             "activated":false,"activation_error":null}]}
// claim 与 claim/manual（同形）
{"outcomes":[{"account_id":"…","account_name":"…","ok":false,
              "message":"凭证失效，请重新授权"}],"summary":{"ok":0,"fail":1}}
```

> **注意**：这不是说 claim 永远不出站 —— 只是**本轮样本里**（账号 invalid）不出站。
> 真正领取成功的路径（含验证码换票）属未覆盖，见第九节。

## 六、账号对象形状（入站旁录）

`accounts/refresh` 的 `account` 字段**键顺序固定**（22 键）：

```
id, name, provider, mode, token_masked, enabled, status,
quota, plan, plans, use_count, fail_count, risk_strikes, recent_results,
last_used_at, last_checked_at, cooling_until, last_error, created_at,
fingerprint, install_id, installed_at
```

- `fingerprint` 7 键：`platform, arch, os_version, language, timezone, screen, device_mid`。
- `token_masked` 形态：`前 8 字符 + … + 后 6 字符`（样本 `eyJhbGci…000000`）。
- `last_checked_at` / `created_at` / `installed_at` 是 **unix 浮点秒**；未设置为 `null`。
- `plans` 是**数组**，`quota` / `plan` 是**对象**（失效时都是空）。

## 七、与 A4 出站头的差异（真实不同，别统一）

| | A4 转发（`/v1/messages`） | A5 额度查询 |
|---|---|---|
| `X-Device-Mid` 等全套指纹头 | **没有** | **有** |
| `X-Title` / `X-Release-Channel` | 没有 | **有** |
| `User-Agent` | `ZCode/3.14.4` | `ZCode/3.14.4`（相同） |
| `X-Zcode-Agent`（A4 有） | 有 | **没有** |

> 即：**转发路径**用「网络身份头」，**额度查询**用「完整客户端指纹」。
> 实现时不要用同一套头去打两类出站。

## 八、脱敏规则（本目录）

依据 [`../README.md`](../README.md) 的两层规则，本目录另有几处**针对性**处理：

| 位置 | 处理 | 理由 |
|---|---|---|
| 请求侧 `X-Request-Id` | **保留原值** | 它是**契约**（三条并发共用同一值、逐批不同） |
| 响应侧 `X-Request-Id` / `Eagleid` | `→ <redacted:hex32>` | 上游**逐次生成**的追踪号 |
| `Set-Cookie` 的取值（`acw_tc` / `cdn_sec_tc` / `visitor_id`） | `→ <redacted:value>` | 持久跟踪值 |
| `Authorization` | `→ <redacted:bearer>` | 凭据 |
| 请求头 `X-Device-Mid`、body 里 `fingerprint.device_mid` | `→ <redacted:device-mid>` / `<redacted:value>` | 逐安装生成的设备标识 |
| body 里 `flow_id` / `state` / `poll_token` / `logid` / `install_id` / `event_id` | `→ <redacted:value>` | 逐次生成 |
| `authorize_url` 里的 `client_id=client_P8X5CMWmlaRO9gyO-KSqtg` | **保留** | 公开的 OAuth 客户端标识，**不是**凭据；保留才能核对授权 URL 形状 |
| `token_masked` | 原样（靶机已自行掩码） | 本就非原值 |

> 残留扫描（JWT / 64hex / 32hex / UUID / cookie 值）在本目录应**只剩请求侧 `X-Request-Id`**。

## 九、未覆盖（不凭猜测补全）

以下分支本轮**没有**采到，实现时凡不能实测的**必须显式报错**，不得近似「试一下」：

1. **OAuth 成功分支**：`poll` 的 `ready` / `finished` 形状、`authorize_url` 回跳拿到 `code`
   后的换票响应 —— 需要**真实账号**配合授权，本轮无。
2. **`poll` 的 `failed` 分支**：只诱发到 HTTP 429（限流），未拿到 `status=failed` 的语义态。
3. **额度查询的 `200` 成功体**：`usage` / `billing/*` 在凭据**有效**时的响应结构，
   以及它如何映射到账号对象的 `quota` / `plan` / `plans`。本轮全是 401/404。
4. **`claim` 的领取成功路径**：含验证码换票、`plans` 填充、`activation_error` 的取值。
5. **冷却 / 退避的精确计时**：`cooling_until` 与 `risk_strikes` 的推进规则（A4 采到过
   网关侧的冷却，但**管理侧额度查询**的退避未逐一钉死）。
6. **`quota_refresh_interval` 的窗口语义细节**：已知默认 1800、置 0 可立即重查、
   进程启动快照；但「窗口从哪一刻起算」未钉死。

## 附：复现

```bash
# 依赖：靶机在 D:/AiWork/ZCode/zcode2api（含 cli.py 与 .venv）；MITM 由仓库内 Go 源码现编
go build -o /tmp/mitmupstream.exe ./tools/mitmupstream
MITM_EXE=/tmp/mitmupstream.exe python tools/capture_admin_outbound.py \
    --target-dir D:/AiWork/ZCode/zcode2api --emit --log-connects
```

采样器以**独立临时数据目录**（`%TEMP%/zcode_a5_capture`）+ **注入的假 JWT 账号**
运行靶机，全程只对本机回环端口发管理请求，不碰任何真实账号；
`--emit` 落盘前按第八节脱敏，控制台输出同样脱敏（**绝不回显凭据**）。
