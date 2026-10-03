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

## 已知未覆盖的分支（后续采样时补）

账号池为空，因此下列分支本轮**未能采到**，实现时不得凭猜测补全，需在拿到真实账号后
重新采样：

- **有账号时的成功链路**：`/v1/messages`、`/v1/chat/completions` 的 200 / 流式响应形态；
  多账号轮询、额度耗尽换号、风控冷却（3012 / 405）的实际回执。
- **额度相关**：`/admin/api/accounts/{id}/refresh`（JWT 账号的真实 `quota` / `plan` 结构）、
  `/admin/api/claim/preview` 的 `plans[]` 结构、`/admin/api/claim` 的成功 `outcomes[]`。
- **OAuth 完成链路**：`/admin/api/login/poll/{flow_id}` 的 `ready` 分支（含 `account` 视图）
  与 `failed` 分支的 `message` 文案。
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
