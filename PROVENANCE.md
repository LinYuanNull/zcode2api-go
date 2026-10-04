# 实现依据登记（PROVENANCE）

本文件登记本仓库各项行为的**依据来源**。它是「独立实现」这一主张的证据链：
每一处与上游可观察行为一致的地方，都应当能对应到一条**采样记录**（真实请求 / 响应
样本），而不是「照着一份 Python 源码写出来」。

## 原则

1. **上游源码不进入本仓库。** `dengyie/zcode2api`（AGPL-3.0）只作为本机靶机运行；
   本仓库不包含、不复制、不翻译其源码。
2. **依据是「可观察行为」，不是「源码结构」。** 样本记录的是 HTTP 请求与响应
   （含错误分支），据此实现；模块划分按本项目自身需要决定，不照搬上游文件切分。
3. **样本只保留结构。** `docs/contract/` 下的样本去掉凭据、Cookie、账号标识与一切
   个人数据，只留字段名、类型、状态码与错误码这类结构性信息。
4. **可复现。** 每条样本都应能用靶机在给定输入下重新采到。

## 靶机

| 项 | 值 |
|---|---|
| 上游项目 | `dengyie/zcode2api` |
| 上游许可 | AGPL-3.0 |
| 靶机位置 | 开发机本地（**不进本仓库**） |
| 靶机版本 | `zcode-hub v2.6.8`（前端 `2.6.3`）；源码为快照拷贝，**非 git 工作树**，无 commit 可记 |
| 采样范围 | 22 个管理 API + 3 个网关 API，共 25 个路由（含错误分支） |
| 样本格式 | 见 `docs/contract/README.md` |

### 采样环境

- 靶机以**独立临时数据目录**启动（`ZCODE_DATA_DIR=<tmp>`、`ZCODE_ADMIN_KEY=1234`），
  采样中新增 / 导入的账号只落在临时库，**不触碰开发机上真实的 `data/accounts.db`**。
- 后台密码为 `1234`（非默认值，`admin_key_is_default` 因此在真实靶机上为 `false`；
  样本 `17-settings-get.GET.json` 采自临时库，该字段为 `true`，属**环境差异**，非结构差异）。
- 账号池为空 ⇒ 账号相关路由采到的是**空态**与**不存在**分支；网关路由采到的是
  **无可用账号**分支（`503 no_available_account`）。这些正是首版实现必须复现的分支。
- 采样时机：`2026-10-03 22:55`（本地时间）。

### 重采样（2026-10-03 23:55）—— 采样器保序缺陷

**问题**：初版采样器把响应体 `json.Unmarshal` 到 `map[string]any` 后再编码，而 Go 编码 map
会**按字典序**输出 —— 样本里的键顺序是工具的产物，不是上游的真实顺序。键顺序是可观测契约
（A2 的落盘 `data` 与 `/admin/api/accounts` 键序都不是字典序，且被逐字节校验），
所以这批样本当时**不足以**作为「真实响应体」的证据。

**修复**：`tools/samplecontract` 改为**流式 Token 按原序清洗**（`sanitizeOrdered` /
`writeSanitized`），并先紧凑编码再 `json.Indent`（因为 `RawMessage` 的缩进会被 `Encoder` 丢掉）。

**重采样**：全部 43 条样本已用修复后的采样器重新采集（同样的隔离临时数据目录）。
核对方式：逐文件对比新旧样本，**39 个文件只有键顺序变化**，另 4 个文件含合法的动态值
（`ts` / `created_at` / `exported_at` 与随机指纹字段）。新样本的键顺序与 A2 的**原始响应体夹具**
（`docs/contract/store/fixtures/`，从未经过 Go 编码）**完全吻合**。

**教训（已写进 `docs/contract/README.md` 的硬规矩）**：契约采样器必须保序。

### 复现方式

```bash
# 1) 起靶机（独立临时库，端口 3000，后台密钥 1234）
cd <上游靶机目录>
ZCODE_PORT=3000 ZCODE_DATA_DIR=<tmp> ZCODE_ADMIN_KEY=1234 .venv/Scripts/python.exe cli.py serve

# 2) 采样（工具随本仓库分发，见 tools/samplecontract）
cd <本仓库>
go run ./tools/samplecontract -base http://127.0.0.1:3000 -admin-key 1234 -out docs/contract
```

采样工具会把凭据、账号 id、设备指纹、OAuth 会话标识按**键规则 + 取值规则**两层
脱敏后落盘（规则见 `tools/samplecontract/main.go` 的「脱敏」一节）。

## 采样记录

> 每个路由一条。样本文件名相对 `docs/contract/`；同一路由的多条样本对应不同分支
> （成功 / 参数错误 / 未授权 / 资源不存在 / 上游异常）。

| 路由 | 方法 | 样本（相对 `docs/contract/`） |
|---|---|---|
| `/admin/api/verify` | GET | `admin/01-verify-ok.GET.json`<br>`admin/01-verify-unauthorized.GET.json` |
| `/admin/api/accounts` | GET | `admin/02-accounts-empty.GET.json`<br>`admin/02-accounts-one.GET.json` |
| `/admin/api/status` | GET | `admin/03-status.GET.json` |
| `/admin/api/accounts` | POST | `admin/04-accounts-add-badprovider.POST.json`<br>`admin/04-accounts-add-notokens.POST.json`<br>`admin/04-accounts-add-ok.POST.json` |
| `/admin/api/accounts` | DELETE | `admin/05-accounts-delete-ok.DELETE.json`<br>`admin/05-accounts-delete-none.DELETE.json` |
| `/admin/api/accounts/{account_id}` | PUT | `admin/06-account-edit-ok.PUT.json`<br>`admin/06-account-edit-404.PUT.json` |
| `/admin/api/accounts/{account_id}/enabled` | POST | `admin/07-account-enabled-ok.POST.json`<br>`admin/07-account-enabled-404.POST.json` |
| `/admin/api/accounts/{account_id}/fingerprint/rotate` | POST | `admin/08-account-fingerprint-ok.POST.json`<br>`admin/08-account-fingerprint-404.POST.json` |
| `/admin/api/accounts/refresh` | POST | `admin/09-accounts-refresh-all.POST.json` |
| `/admin/api/accounts/{account_id}/refresh` | POST | `admin/10-account-refresh-nonjwt.POST.json`<br>`admin/10-account-refresh-404.POST.json` |
| `/admin/api/login/start` | POST | `admin/11-login-start.POST.json` |
| `/admin/api/login/poll/{flow_id}` | GET | `admin/12-login-poll-unknown.GET.json` |
| `/admin/api/claim/preview` | GET | `admin/13-claim-preview-empty.GET.json` |
| `/admin/api/claim` | POST | `admin/14-claim-empty.POST.json` |
| `/admin/api/claim/captcha-config` | GET | `admin/15-claim-captcha-config.GET.json` |
| `/admin/api/claim/manual` | POST | `admin/16-claim-manual-missing-id.POST.json`<br>`admin/16-claim-manual-404.POST.json` |
| `/admin/api/settings` | GET | `admin/17-settings-get.GET.json` |
| `/admin/api/settings` | PUT | `admin/18-settings-put-empty-key.PUT.json`<br>`admin/18-settings-put-ok.PUT.json`<br>`admin/18-settings-put-reset-gwkey.PUT.json` |
| `/admin/api/export` | GET | `admin/19-export.GET.json` |
| `/admin/api/import` | POST | `admin/20-import-ok.POST.json` |
| `/admin/api/monitoring` | GET | `admin/21-monitoring.GET.json` |
| `/admin/api/monitoring/clear` | POST | `admin/22-monitoring-clear.POST.json` |
| `/v1/models` | GET | `gateway/23-models-noauth.GET.json`<br>`gateway/23-models-wrongkey.GET.json`<br>`gateway/23-models-ok.GET.json` |
| `/v1/messages` | POST | `gateway/24-messages-noauth.POST.json`<br>`gateway/24-messages-badjson.POST.json`<br>`gateway/24-messages-noaccount.POST.json` |
| `/v1/chat/completions` | POST | `gateway/25-chat-noauth.POST.json`<br>`gateway/25-chat-badjson.POST.json`<br>`gateway/25-chat-noaccount.POST.json` |

合计 **25 个路由 / 43 条样本**（覆盖核查脚本见 A1 记录：期望 25、实际 25，无缺失无多余）。

## 落盘契约采样（A2）

HTTP 样本（上表）记录的是**跨进程可观察的 HTTP 行为**；A2 另需一类证据：
靶机**写出的文件**。因为 Go 实现要接管用户现有的 `data/accounts.db`，
「能读它、且不改动它的语义」是必须被证明的。

| 项 | 值 |
|---|---|
| 采样对象 | 靶机在独立临时数据目录里写出的 `accounts.db`（`sqlite_master` + 原始行 + 原始 `data` 字符串） |
| 采样方法 | 用靶机**自己的 HTTP 管理 API** 造数据（不读源码、不直接写库），然后读它写出的文件 |
| 规则归纳 | 逐项变化的请求 + 回读响应（slug / 默认名 / 去重 / 掩码 / 强转 / 默认值 / 状态联动 / 列表顺序） |
| 产出 | `docs/contract/store/`（`schema.sql` / `account-data-shape.json` / `fingerprint-shape.json` / `observations.md`） |
| 夹具 | `docs/contract/store/fixtures/`（`target.db` 36864 B + 4 个原始响应体），由 `tools/samplefixture/` 生成并入库 |
| 采样时机 | `2026-10-03 23:30`（本地时间） |

**夹具的合成性**：`target.db` 里的 6 条账号全部由采样脚本用**合成凭据**创建
（`api_key` 形如 `fixture-token-NNNN`；`gateway_key` 为 `fixture-gw-key-abcdef`；
`admin_key` 为默认值 `1234`）。设备指纹的 `device_mid` / `install_id` 是靶机在
临时库上随机生成的，与任何真实用户无关。**不含任何真实凭据或个人数据。**

**采样环境与 A1 相同**：`ZCODE_DATA_DIR=<tmp>`、`ZCODE_ADMIN_KEY=1234`、端口 `13011`，
靶机版本 `zcode-hub v2.6.8`（前端 `2.6.3`）。

**复现方式**：

```bash
# 起靶机 → 造数据 → 抓库与响应体，一次产出全部夹具（内部用独立临时数据目录）
cd <本仓库>
go run ./tools/samplefixture -zcode <上游靶机目录> -out docs/contract/store/fixtures
```

**验收测试**（`internal/store`、`internal/models`、`internal/settings`、`internal/constants`）
以夹具为输入做**双向读校验**：读库 → 我们的类型 → 重新编码，与靶机原始 `data`
**紧凑化后逐字节相同**；`View()` 与靶机原始响应体逐字段相同；打开 + 全量读之后
库文件与每行 `data` 不变。断言 ↔ 规则 的对应表见 `docs/contract/store/observations.md` §6。

## 管理 API 实现依据（A3）

A3 实现 22 条管理路由（路由 + 鉴权 + settings 读写）。依据仍是**运行中靶机的可观察行为**，
本节的每条结论都附了「怎么测出来的」，便于复核。

### 采样环境的两处误记（A3 更正）

A2 把**这份部署 `.env` 的取值**当成了**代码默认值**，两条都已在
`docs/contract/store/observations.md` §3.1/§3.2 更正：

| 项 | A2 记录 | 实测（A3） | 判据 |
|---|---|---|---|
| `admin_key_is_default` | `= (admin_key == "1234")` | `=`（库里的 `admin_key` == **本进程配置给的默认密码**）；配置来自 `ZCODE_ADMIN_KEY`，未设回落 `zcode` | ① 库 `7777` + 以 `ZCODE_ADMIN_KEY=8888` 重启 → 用 7777 能进但 `is_default=false`；② 库 `7777` → PUT `8888` → 以 env `8888` 重启 → `true` |
| `claim_round_interval` 默认 | `0` | **`3600`** | 把 `.env` 移开、不设 `ZCODE_CLAIM_ROUND_INTERVAL`，全新数据目录首启 → `3600`；设 `222` → `222` |

复现这类实验**必须把靶机的 `.env` 移开** —— 它的 `ZCODE_ADMIN_KEY=1234` 与
`ZCODE_CLAIM_ROUND_INTERVAL=0` 正是那两条误记的来源。

### 配置链路（实测）

- 上游 `serve` **没有任何命令行参数**（`cli.py serve --help` 会直接去启动服务），
  配置全走环境变量 / `.env`。
- `ZCODE_ADMIN_KEY` / `ZCODE_QUOTA_REFRESH_INTERVAL` / `ZCODE_ACCOUNT_CONCURRENCY` /
  `ZCODE_CLAIM_ROUND_INTERVAL` 都是**首启默认值**：首启 `INSERT OR IGNORE` 写库，之后**以库为准**。
- **`ZCODE_GATEWAY_KEY` 不生效**（三条独立探测：进程环境、`.env`、运行期 `/v1/models` 不带凭证仍 200）。
  所以 `gateway_key` 的初值恒为 `""`，只能经 `PUT /admin/api/settings` 设置。

### 逐条实测钉死的判据

| 判据 | 结论 | 怎么测的 |
|---|---|---|
| `mode` 判定 | **仅 `provider == "zai"` 且凭据恰好含 2 个点 → `jwt`**，否则 `apiKey` | 对运行中的靶机逐条探测：`..`（只有点、无内容）也判 jwt；同一串 JWT 交给 `bigmodel` 被存成 `apiKey` ⇒ provider 是硬条件 |
| 导入格式 | 只接受**对象条目** `{name, mode, secret}`；字符串数组形态（`{"providers":{"zai":["sk-a"]}}`）→ **500** | 两种 payload 各打一次 |
| 鉴权位置 | **在路由匹配之后**：未知路径（`/admin/api/nope`）不带凭证也是 **404 `{"detail":"Not Found"}`**，不是 401 | 不带 `Authorization` 请求未知路径 |
| 密钥掩码 | 空串 → `""`；`len ≤ 8` → `••••`；否则 `前 4 + '…' + 后 4` | 逐长度取值回读 |
| 空容器 | 空数组/空对象写 `[]` / `{}`，**不写 `null`** | 逐路由读空态响应 |
| 业务失败 | 用 **200** 承载（`ok:false`），不能只看状态码 | `10-account-refresh-nonjwt` |

### 键顺序（可观测契约）

样本里的键顺序**就是上游的真实顺序**（2026-10-03 重采样后恢复，见上）。
`docs/contract/admin/SPEC.md` 第二部分的骨架曾按字典序手写，与样本矛盾 ——
已用 `tools/spec_reorder.py` 按样本重排，并在 CI 里加了一步
`python3 tools/spec_reorder.py --check`，防止再次漂移。

### 未实现的分支一律显式报错

需要上游 OAuth / 额度接口的分支（JWT 额度刷新、定时领取、登录发起）在 A3 阶段
**一律显式失败**（`501 尚未实现` / `502 登录初始化失败: …（需要上游 OAuth，属于 A5）`），
**绝不伪造成功、绝不伪造 `flow_id`**。端到端断言同时覆盖面板文案与代理链路状态码。

### 验收（全部来自真实运行）

| 验收项 | 结果 |
|---|---|
| 样本回放（43 条 + 空池分支逐字节） | 全绿 |
| 上游自带前端面板（`tools/e2e_panel.py`，真浏览器 CDP 驱动 `frontend/`） | 38/38 |
| ModelMux 集成脚本（`test/verify_zcode_accounts.py`，默认假网关） | 44/44（基线未破） |
| 同上，`ZCODE_UPSTREAM_EXE` 指向本实现 | 47/47 |
| `gofmt -l .` / `go vet ./...` / `go test ./...` | 干净 / OK / 全绿 |
| `python tools/spec_reorder.py --check`（SPEC 骨架键序） | 0 块漂移 |

**提交与 CI**：A3 落地于提交 `1696bc9`（`feat(adminapi): A3 管理 API 22 路由`），
CI run [`37141410784`](https://github.com/LinYuanNull/zcode2api-go/actions/runs/37141410784) **success**
（CI 已加「契约键序」步骤：`python3 tools/spec_reorder.py --check`）。

## 转发链路实现依据（A4）

A4 的判据**不是** `docs/contract/*.json`（那些是**入站**的请求/响应样本），而是**出站**行为：
出站请求体、出站头、上游响应经网关后的入站回执、以及控制台记号序列。依据全部集中在
`docs/contract/outbound/`：

- **`behavior.md`** —— 转发行为契约（两个入口的语义差异、响应头规则、响应形状逐字节、
  上游错误分类与状态机、`>>>`/`[~]`/`<!>` 记号约定）。
- **`observations.md`** —— 采样观察（端点、代理语义、字段映射、调度行为、分类边界）。
- **`fixtures/`** —— 逐场景的出站请求体、路由记号探针、chat 重建的未采样边界表。

采样方法：本地 MITM 假上游（`tools/mitmupstream`）拦截真实转发，把上游响应作为**输入参数**
注入，从而在**账号池为空**的情况下也能穷举错误分支与切换/冷却路径。

### 与我方自建 harness 的行为对照

`tools/behavior_diff.py` 起**同一套**假上游，分别拉起 Python 参考实现（靶机）与我方 Go 实现，
逐场景比较：响应头白名单、语义响应体、出站请求体**逐字节**、响应体**逐字节**、记号序列。
易变字段（`id` / `created` / `created_at` / `logid` / `install_id` / `event_id`）掩码为 `<volatile>`。

**结果：23 个场景中 4 个存在差异，且 4 个的差异全部只有易变字段**（`ok-openai` /
`ok-openai-stream` / `chat-stream-from-json` / `stream-string-false`）——即 chat 转换路径里
由时间戳与新生成 id 造成的必然差异，非语义差异。其余 19 个场景（含全部 `/v1/messages`
成功/错误/SSE、`upstream-401/400/403/429/429-retryafter/500/abort/200-badjson/200-error`、
`fail-then-ok`、`configs-down`、`no-account`、`chat-sse-on-nonstream`、`ct-passthrough`、
`route-preview-multiturn`、`model-unknown`）逐字节「一致」。

### 有意偏离（参考实现是缺陷，本实现不照抄）

`/v1/chat/completions` 重建遇到的**未采样边界**（如 `role: system`、`tools`/`temperature`
等 OpenAI 字段、`max_tokens` 缺失）一律按「白名单重建、其余丢弃」的**既定口径**处理，
不猜默认值；`model` 非字符串的入站请求参考实现是 500 崩溃，本实现改为 400 + 明确文案。
逐条见 `docs/contract/outbound/behavior.md` §六之二、§六之三。

### 验收（全部来自真实运行）

| 验收项 | 结果 |
|---|---|
| 契约回放（A3 的 43 条 + 空池分支，合计 45 个子用例） | 全绿 |
| 行为对照 harness（`tools/behavior_diff.py`，23 场景 × Python/Go） | 19 一致 + 4 仅易变字段 |
| 上游自带前端面板（`tools/e2e_panel.py`，真浏览器 CDP 驱动 `frontend/`） | 38/38（A4 未破） |
| `gofmt -l .` / `go vet ./...` / `go test ./...` | 干净 / OK / 全绿 |

## 管理侧出站实现依据（A5）

A5 的判据是**管理面的出站**（面板 → `zcode.z.ai`）。它与 A4 的转发出站并列，但**头集合
彼此不同、不能统一**：额度查询带**全套客户端指纹头**、OAuth 极简（只有 Authorization +
UA）、转发链路又是另一套。依据全部集中在 `docs/contract/outbound-admin/`：

- **`observations.md`** —— 九节实测记录：端点、**「管理侧出站也走代理」的更正**、OAuth
  令牌来源与 `flow_id` 归属、poll 无本地门控、额度三条并发共用一个 `X-Request-Id`、
  claim 三条零出站、与 A4 的头差异、脱敏规则、未覆盖分支。
- **5 个样本**（`01-oauth-init` … `05-plan-billing-balance`）—— 逐字请求头 + 解压后响应体。
- **`fixtures/`** —— 10 条出站请求旁录（含 `_excluded` 记下的 A4 噪声）+ 13 条入站响应旁录
  （覆盖 `refresh` 的两种形态：`result` 真查 / `message` 缓存）。

采样方法：把靶机的 `HTTPS_PROXY` 指向 `tools/mitmupstream`（自签 CA + 叶子证书终结 TLS），
`SSL_CERT_FILE` 指向该 CA，靶机跑在独立临时数据目录 + 注入的假 JWT 账号上。
**管理侧出站同样走代理**这一条正是本轮实测确认的 —— A4 时期曾按「billing 必须直连」的
假设在 `agent` 里留过一个 `Direct()` 变体，本轮实测推翻并**删除了它**（没有实测支持的分支
不留，免得将来有人顺手用它把流量打直连、在上游风控前暴露真实出口 IP）。

### 逐条实测钉死的判据

| 判据 | 实测结论 |
|---|---|
| `flow_id` 归属 | 管理侧 `login/start` 返回的 id == 上游 `init` 的 `data.flow_id` == 上游 poll 路径里的 id，**三值 SHA-256 全等**（32 位 hex）⇒ 会话标识所有权在**上游**，本地不自生成 |
| `login/start` 对上游的变换 | `expires_in` **固定 300**（上游给的是 `expires_at` 时间戳）；丢弃 `poll_token` / `logid` / `poll_interval_sec` |
| OAuth Bearer | 64 位小写 hex，**进程内随机生成、不落盘**；`init` 的请求 Bearer == 响应 `poll_token`，后续 `poll` 复用**同一个** |
| poll 节流 | **没有本地时间门控**：每次 poll 逐次打上游。上游偶发 429（非确定性）；**一旦某次出站失败，该 flow 后续 poll 不再打上游**（零出站）仍回缓存状态 |
| 未知 flow | 本地直接回 `expired`（**零出站**），且 HTTP 是 **200** 不是 404 |
| 额度查询并发 | `usage` + `billing/current` + `billing/balance` 同毫秒并发（≤1ms），**共用同一个 `X-Request-Id`**（进程级、逐批生成、uuid4） |
| 凭据失效的状态码 | **不一致**：`usage` → **404**（gzip 的 `404 page not found` 文本）、`billing/current` → **401**、`billing/balance` → **401**（后两者空体、无 `Content-Encoding`） |
| 指纹头平台映射 | `X-Os-Category` = `macos`(darwin) / `windows`(win32)；`X-Platform` = `<platform>-<arch>`；`X-Os-Version` 与 platform 严格同域；`X-Release-Channel` 恒 `stable`；`X-Title` 逐字 `Z Code@electron` |
| GET 上的 `Content-Type` | 额度查询是 GET，却**真的带** `Content-Type: application/json`（照发，不「修正」） |
| 额度查询触发条件 | 只对 `status=active` 的 **JWT** 账号查上游；触发点 = `POST /admin/api/accounts`（主）、启动自刷、`refresh`；转 `invalid` 后**永不再查**（`refresh` 回缓存、零出站） |
| `refresh` 的两种响应形态 | FRESH（真查上游）走 `{"ok":false,"result":{"error":…}}`；CACHED（缓存 / invalid）走 `{"ok":false,"message":…}`。**判据是键名**（`result` vs `message`），两者 `account.status` 都是 `invalid` |
| claim 出站 | `claim/preview` / `claim` / `claim/manual` / `claim/captcha-config` **全部零出站** |

### 一处必须自己解的坑（Go 特有）

出站头**必须显式**发 `Accept-Encoding: gzip, deflate`（契约逐字固定）。而 Go 的 `Transport`
只在「**它自己**加上的 `Accept-Encoding`」上做透明解压 —— 显式设值后它把压缩字节**原样**
交出来。实测上游这几个端点回 gzip（连 404 的 `404 page not found` 都是 gzip 的），
所以 `agent.ReadBody` 必须按 `Content-Encoding` **自行解压**（deflate 与 httpx 同语义：
先按 zlib 试、失败再按裸 deflate；未支持的编码**明确报错**而不是把压缩字节当明文）。

### 验收（全部来自真实运行）

| 验收项 | 结果 |
|---|---|
| 真实二进制端到端 `POST /admin/api/login/start` | `{"flow_id","authorize_url","expires_in"}` 键序正确；`flow_id` 32 位 hex；`expires_in` = 300；`authorize_url` 来自上游 |
| 真实二进制端到端 poll（未知 flow） | `{"status":"expired"}` + HTTP 200，且 MITM 抓包记录数**不变**（零出站） |
| 真实二进制端到端 poll（真实 flow，连续 3 次） | 每次 `{"status":"pending"}`，MITM 记录数 2 → 5（**+3 = 逐次都打上游**） |
| 无凭证 | 401（鉴权在路由匹配之后，仍生效） |
| **出站逐字节对照**（把本实现的 `HTTPS_PROXY` 指向 `tools/mitmupstream` 抓自己） | `init`：POST `/api/v1/oauth/cli/init`，头**恰好** 7 个（Accept / Accept-Encoding / Authorization / Connection / Content-Length / Content-Type / User-Agent），`User-Agent: python-httpx/0.28.1`，体逐字 `{"provider":"zai"}` —— 与样本 `01` 一致 |
| 同上，`poll` | GET `/api/v1/oauth/cli/poll/<flow_id>`，头**恰好** 5 个（无 `Content-Type`），Bearer 与 `init` **同值**、64 hex —— 与样本 `02` 一致 |
| 单测（`internal/agent` 8 例 + `internal/oauth` 12 例） | 全绿（假上游 `httptest` + `SetA5BaseForTest`，不依赖 z.ai 可达） |
| 契约回放（A3 的 43 条 + 空池分支） | 全绿（`11-login-start` 的「已知分歧」已消除，改为 volatile `flow_id`/`authorize_url`） |
| **A5-3 真实二进制端到端**（`tools/e2e_quota.py`） | **31/31**：FRESH / CACHED 两形态逐字节、同批 `X-Request-Id` 相同、出站头**恰好** 17 个、`invalid` 之后零出站、`refresh all` 汇总 |
| **A5-4 真实二进制端到端**（`tools/e2e_claim.py`） | **34/34**：两条沙盒（失效 / `active`）；失效分支三条路由与旁录**逐字节一致**且**之后零出站**；`active` 分支三条路由 **501** 且说明点明账号与 `status=active`；`claim` 子命令在无成功时非零退出 |
| `gofmt -l .` / `go vet ./...` / `go test ./...` | 干净 / OK / 全绿 |

**提交与 CI**：A5-1 的实测订正落地于 `2aa8b6d`，A5-2 落地于 `eaaf90a`
（`feat(A5-2): 登录链路真实落地`），A5-3 落地于 `497e6cf`
（`feat(A5-3): 额度查询真实落地`）。CI run
[`37157384935`](https://github.com/LinYuanNull/zcode2api-go/actions/runs/37157384935) **success** ——
`gofmt` / `go vet` / `go build` / `go test` / 契约键序五步全绿。
其中 `go test` 跑在 **ubuntu-latest** 上，正好验证了「契约回放已**不依赖 z.ai 可达性**」
这一点（否则登录样本在 CI 上必然随机失败）。

### 领取链路实现依据（A5-4）

领取的判据不是「出站样本」，而是**入站旁录**：本轮三条 claim 路由**零出站**，
所以契约体现在响应体本身 —— `fixtures/admin-responses.json` 的 `claim/preview` /
`claim` / `claim/manual` 三条 `body_text`，外加 `docs/contract/admin/13-*` / `14-*` /
`15-*` / `16-*` 四组样本。实现（`internal/claim`）的范围就由此划定：

| 分支 | 依据 | 本实现 |
|---|---|---|
| 候选 = JWT 账号 | `13-*` / `14-*` 的 notes（「无 JWT 账号时为空」）+ `10-account-refresh-nonjwt` | 实现 |
| 池里无候选 → `{"preview":[]}` / `{"outcomes":[],"summary":{"ok":0,"fail":0}}` | `13-*` / `14-*` 逐字节 | 实现 |
| **凭据失效**（`status=invalid`）的 preview 行 | 旁录 `claim/preview` 逐字节：`plans:[]`、`error`、`activated:false`、**`activation_error:null`** | 实现 |
| 同上，`claim` 与 `claim/manual` 的回执 | 旁录两条 `body_text` **逐字相同**；`summary` 由回执逐条数出 | 实现 |
| `claim/manual` 缺 `account_id` → 400；非 JWT / 不存在 → 404 | `16-claim-manual-{missing-id,404}` | 实现（在调用领取器**之前**判） |
| `claim/captcha-config` | `15-*`：`{enabled, scene_id, region, prefix}` | 实现为**诚实降级**的空配置（未接 A6 时 `enabled:false` + 空串） |
| **`status=active` 的真实领取** | **无样本**（下节第 4 条） | **显式报错（501）** |

三条实施要点（都是「看起来能省、省了就错」的地方）：

1. **`activation_error` 必须是「键在、值为 `null`」，不能是 `omitempty`**。写成
   `string` + `omitempty` 时空值会把**整个键**省掉，形态与旁录不符。字段类型是
   `*string` 且不带 `omitempty`，`adminapi/claim_test.go` 有一条断言专门钉它。
2. **`plans` 必须是 `[]` 而不是 `null`**：走 `[]Plan{}`（非 nil 空切片）。
3. **只要有一个候选不是 `invalid` 就整体报错，不回部分结果**。把「有样本的行」与
   「猜的行」混在同一个数组里返回，调用方无法分辨哪部分可信；宁可 501 并说明是哪个
   账号、什么状态卡住了。

`account_ids` 的过滤语义：`{"account_ids":[…]}` 是**过滤器**，空 / 缺省 = 全部候选。
**这是推断**（不是样本）：`14-*` 的请求体是 `{}` 而池恰好是空的，两种情况都产出空 outcomes，
分不出「空 = 全部」还是「空 = 一条都不领」。判据取自上游 Python 侧把 `account_ids` 当可选
过滤器的写法（`set(body.get("account_ids") or [])`，旁证见协同仓库 ModelMux 的
`test/fake_zcode.py` 对同一契约的仿真），且顺序按**池内顺序**而非请求顺序。
**待真实账号补采时校准。**

同理，`POST /admin/api/claim` 的**成功回执形状**（`plan_name` / `grants` / `code` / `next_at`
的键序）也没有样本。协同仓库的 `test/fake_zcode.py` 里有一份「按上游仿真」的写法，其中
1005 分支的键序是 `…ok, code, message, next_at`，与 `14-*` notes 记的
`…ok, plan_name?, grants?, message?, code?, next_at?` **不一致**。两份都不是样本，
故**不据此实现**，登记在此供补采时逐项核对 —— 这正是「不凭猜测补全」要防的情形。

**`claim_round_interval` 目前不驱动任何代码。** 设置项本身有样本（默认 3600，且能被
`ZCODE_CLAIM_ROUND_INTERVAL` 覆盖为首启值），但**没有任何样本显示存在一个周期性领取循环**：
本轮三条 claim 路由零出站、且只在被请求时才产生响应；`internal/quota` 那边有实测到
「启动自刷」这个触发点（observations.md 4.4 第 2 条），claim 侧**没有对应证据**。
因此不实现定时器 —— 一个不会产生任何可观测行为的后台循环既无法验收，也会在
`invalid` 账号上安静地空转。待补采（真实账号 + 到点观察出站与 `next_at`）后再定。

### A5 未覆盖（不凭猜测补全）

1. **OAuth 成功分支**：poll 的 `ready` / `finished` 形状、回跳 `code` 换票响应 —— 需真实账号。
   因此本实现只**原样透传** `status`，**不伪造账号**（`Account` 落库留给拿到真实账号后的步骤）。
2. **poll 的 `failed` 语义态**：只诱发到 429（限流），未拿到 `status=failed`。
3. **额度查询的 `200` 成功体**：`usage` / `billing/*` 在凭据**有效**时的结构，以及它如何映射到
   账号对象的 `quota` / `plan` / `plans` —— 本轮全是 401/404。**A5-3 已按纪律对该分支显式报错**
   （`ErrSuccessShapeUnsampled` → 501，且**不改账号状态**）。
4. **`claim` 领取成功路径**：验证码换票、`plans` 填充、`activation_error` 取值。
   ⚠️ 本轮更关键的一条：**上游端点表里根本没有领取端点**（`observations.md` 一之表只有
   oauth init/poll 与 zcode-plan usage/billing 共 5 个），所以「往哪发」都不知道 ——
   它比「响应形状未知」更靠前一步。**A5-4 已按纪律对该分支显式报错**（`ErrUnsampled` → 501）。
   补采前必须先有**真实账号**走完 OAuth（第 1 条），再让该账号真的领一次。
5. **`quota_refresh_interval` 的窗口起算点**：已知默认 1800、置 0 立即重查、进程启动快照。

### 尚未接通的接缝

- **`login` 子命令**：管理面板的登录链路已通（`/admin/api/login/start` + `poll`），
  但 `zcode2api-go login` 这个 CLI 子命令未接通 —— 缺的是第 1 条（`ready` 之后凭据落库），
  它未采样，所以 CLI 不假装能完成登录。
- **`internal/captcha`**：仍是占位实现（`captcha-config` 返回空配置），求解器属 A6。

## 附：仍待补采的未覆盖分支（A2 / A4）

> 这两组是 **A2 / A4** 留下的空档，与上一节的 A5 无关 —— 列出它们是因为**同一条「不凭猜测
> 补全」的纪律**适用于全部阶段。

### A4 未覆盖的分支（当时账号池为空）

A4 采样时账号池为空，因此下列分支当时**未能采到**，实现时不得凭猜测补全，需在拿到真实账号后
重新采样：

- **有账号时的成功链路**：`/v1/messages`、`/v1/chat/completions` 的 200 / 流式响应形态；
  多账号轮询、额度耗尽换号、风控冷却（3012 / 405）的实际回执。
- **额度相关**：`/admin/api/accounts/{id}/refresh`（JWT 账号的真实 `quota` / `plan` 结构）、
  `/admin/api/claim/preview` 的 `plans[]` 结构、`/admin/api/claim` 的成功 `outcomes[]`。
- **OAuth 完成链路**：`/admin/api/login/poll/{flow_id}` 的 `ready` 分支（含 `account` 视图）
  与 `failed` 分支的 `message` 文案。（**额度与领取两条仍未覆盖**；OAuth 的发起/轮询与
  凭据失效分支已在 A5 补齐，见上一节。）
- **验证码**：`/admin/api/claim/manual` 在拿到浏览器端 `verify_param` 后的成功分支。

### 落盘契约（A2）未覆盖的分支

- **`mode="jwt"` 的账号**：只能由 OAuth 登录链路产生（A5）。字段已建模并保留 `jwt_token`。
- **`status` 取 `cooling` / `exhausted` / `invalid`**：需真实账号真实调用失败才会进入（A4）。
  枚举已按 `stats` 的键名登记，**转移条件未采样**。
- **`quota` / `plan` / `plans` / `usage` / `recent_results` 的内部结构**：空账号一律 `{}` / `[]`。
  Go 侧按**不透明 JSON 原样透传**，不解析、不重排。
- **`installed_at` 何时被写入**：空账号为 `null`，保持透传。
- **`stats.calls` / `stats.fail` 的语义**：采样时全库 `use_count` / `fail_count` 均为 0，
  两种解释（求和 / 其它）都能得到 0。Go 侧暂取「求和」，**待 A4 拿到真实调用后校准**。
- **指纹各字段的联合分布**：只能观察到边际池与平台相关性。Go 生成器从观测池取值并保证
  平台一致性；随机量本就不可能逐字节复现，验收只要求「读取既有账号逐字段一致」。
- **`name` 全为标点 / 空白时的 slug 结果**：未采样。Go 侧退到 provider 以保证 id 合法。

## 与上游的关系

- 本仓库是**独立实现**，不是 fork、不是分支、不是移植。
- 不随包分发上游的任何文件或二进制。
- 上游更新不会自动合入；由维护者定期比对上游 release notes 后人工决定是否跟进。
