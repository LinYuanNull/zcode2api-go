# 管理 API 契约规格（SPEC）

依据 `docs/contract/admin/` 下 **34 个**样本（**22 条路由**）逐条归纳；`gateway/` 的 9 个样本仅登记。
键顺序按样本文件（pretty-print JSON）原样保留。所有判断仅来自样本内容，样本未覆盖者明确标注。

> 数量校正：任务书称 admin 有「43 条样本」，实际目录为 **34 个 JSON**（见第三部分异常点 1）。

> **2026-10-03 重采样说明（重要）**：本规格初稿基于**旧样本**，其键顺序**是采样工具的产物**
> （旧采样器把响应体解到 `map[string]any` 再编码，Go 会按字典序输出）。该缺陷已修复
> （`tools/samplecontract` 改为流式 Token 按原序清洗），并**已重新采样全部 43 条样本**。
> 因此本文件里的键顺序**现在就是上游的真实顺序**；第三部分异常点 2、6 与第四部分的
> 「键顺序」提示均已据此更正。核对依据：重采样前后逐文件对比，39 个文件**只有键顺序变化**、
> 4 个文件另含合法的动态值（`ts` / `created_at` / `exported_at` / 随机指纹字段）。

## 第一部分：路由总表

| 路径 | 方法 | 鉴权 | 成功状态码 | 响应体顶层字段（按样本出现顺序） | 样本文件 |
|---|---|---|---|---|---|
| `/admin/api/verify` | GET | Bearer 必需（有 401 证据） | 200 | `status` | `01-verify-ok.GET.json` |
| `/admin/api/accounts` | GET | Bearer（样本均携带；未采未授权分支） | 200 | `accounts`, `providers`, `stats`, `ts` | `02-accounts-empty.GET.json`, `02-accounts-one.GET.json` |
| `/admin/api/status` | GET | Bearer（样本均携带；未采未授权分支） | 200 | `gateway_key_set`, `providers`, `quota_pool` | `03-status.GET.json` |
| `/admin/api/accounts` | POST | Bearer（样本均携带；未采未授权分支） | 200 | `count`, `ids` | `04-accounts-add-ok.POST.json` |
| `/admin/api/accounts` | DELETE | Bearer（样本均携带；未采未授权分支） | 200 | `deleted` | `05-accounts-delete-ok.DELETE.json`, `05-accounts-delete-none.DELETE.json` |
| `/admin/api/accounts/{account_id}` | PUT | Bearer（样本均携带；未采未授权分支） | 200 | `ok` | `06-account-edit-ok.PUT.json` |
| `/admin/api/accounts/{account_id}/enabled` | POST | Bearer（样本均携带；未采未授权分支） | 200 | `ok` | `07-account-enabled-ok.POST.json` |
| `/admin/api/accounts/{account_id}/fingerprint/rotate` | POST | Bearer（样本均携带；未采未授权分支） | 200 | `fingerprint`, `ok` | `08-account-fingerprint-ok.POST.json` |
| `/admin/api/accounts/refresh` | POST | Bearer（样本均携带；未采未授权分支） | 200 | `count`, `skipped_cooling`, `skipped_invalid`, `summary` | `09-accounts-refresh-all.POST.json` |
| `/admin/api/accounts/{account_id}/refresh` | POST | Bearer（样本均携带；未采未授权分支） | 200 | `message`, `ok` | `10-account-refresh-nonjwt.POST.json` |
| `/admin/api/login/start` | POST | Bearer（样本均携带；未采未授权分支） | 200 | `authorize_url`, `expires_in`, `flow_id` | `11-login-start.POST.json` |
| `/admin/api/login/poll/{flow_id}` | GET | Bearer（样本均携带；未采未授权分支） | 200 | `status` | `12-login-poll-unknown.GET.json` |
| `/admin/api/claim/preview` | GET | Bearer（样本均携带；未采未授权分支） | 200 | `preview` | `13-claim-preview-empty.GET.json` |
| `/admin/api/claim` | POST | Bearer（样本均携带；未采未授权分支） | 200 | `outcomes`, `summary` | `14-claim-empty.POST.json` |
| `/admin/api/claim/captcha-config` | GET | Bearer（样本均携带；未采未授权分支） | 200 | `enabled`, `prefix`, `region`, `scene_id` | `15-claim-captcha-config.GET.json` |
| `/admin/api/claim/manual` | POST | Bearer（样本均携带；未采未授权分支） | — | ⚠ 仅错误分支 | `16-claim-manual-404.POST.json`, `16-claim-manual-missing-id.POST.json` |
| `/admin/api/settings` | GET | Bearer（样本均携带；未采未授权分支） | 200 | `account_concurrency`, `admin_key_is_default`, `admin_key_masked`, `admin_key_set`, `claim_round_interval`, `gateway_key_masked`, `gateway_key_set`, `quota_refresh_interval` | `17-settings-get.GET.json` |
| `/admin/api/settings` | PUT | Bearer（样本均携带；未采未授权分支） | 200 | `ok` | `18-settings-put-ok.PUT.json`, `18-settings-put-reset-gwkey.PUT.json` |
| `/admin/api/export` | GET | Bearer（样本均携带；未采未授权分支） | 200 | `exported_at`, `providers`, `version` | `19-export.GET.json` |
| `/admin/api/import` | POST | Bearer（样本均携带；未采未授权分支） | 200 | `count` | `20-import-ok.POST.json` |
| `/admin/api/monitoring` | GET | Bearer（样本均携带；未采未授权分支） | 200 | `entries`, `keep` | `21-monitoring.GET.json` |
| `/admin/api/monitoring/clear` | POST | Bearer（样本均携带；未采未授权分支） | 200 | `ok` | `22-monitoring-clear.POST.json` |

网关路由（仅登记，不细展开）：

| 路径 | 方法 | 鉴权 | 成功状态码 | 响应体顶层字段 | 样本文件 |
|---|---|---|---|---|---|
| `/v1/models` | GET | 网关 Key | 200 | `data`, `object` | `gateway/23-models-ok.GET.json` |
| `/v1/messages` | POST | 网关 Key | 样本未覆盖成功 | — | `gateway/24-*.POST.json` |
| `/v1/chat/completions` | POST | 网关 Key | 样本未覆盖成功 | — | `gateway/25-*.POST.json` |

鉴权说明：仅 `/admin/api/verify` 采到未授权分支（无 `Authorization` → 401）。其余 admin 路由所有样本均携带 `Authorization: <redacted:bearer>`，但**未采未授权分支**，故无法从样本证实其是否必需 Bearer；样本中也不存在任何「公开路由」证据。

## 第二部分：逐路由详情

### 1. GET /admin/api/verify
- 无路径参数；无请求体。
- 成功 200：
```json
{ "status": "ok" }
```
- 错误分支：
  - 401 `{"detail": "缺少鉴权凭证"}` —— 请求不带 `Authorization` 头。样本 `01-verify-unauthorized.GET.json`。
- notes 要点：正确密钥仅返回 `{status:ok}`；缺凭证为 FastAPI 默认 HTTPException 形态 `{detail:…}`。

### 2. GET /admin/api/accounts
- 无参数；无请求体。
- 成功 200 骨架（键序即样本）：
```json
{
  "accounts": [ <account>, ... ],
  "stats": {
    "total": <number>,
    "active": <number>,
    "exhausted": <number>,
    "cooling": <number>,
    "invalid": <number>,
    "disabled": <number>,
    "calls": <number>,
    "fail": <number>
  },
  "providers": [ "zai", "bigmodel" ],
  "ts": <number>
}
```
- `<account>`（public_view，键序即样本）：
```json
{
  "id": "<slug>-<8hex>",
  "name": "<string>",
  "provider": "zai",
  "mode": "apiKey",
  "token_masked": "<masked>",
  "enabled": <bool>,
  "status": "active",
  "quota": {},
  "plan": {},
  "plans": [],
  "use_count": <number>,
  "fail_count": <number>,
  "risk_strikes": <number>,
  "recent_results": [],
  "last_used_at": <number|null>,
  "last_checked_at": <number|null>,
  "cooling_until": <number|null>,
  "last_error": <string|null>,
  "created_at": <number>,
  "fingerprint": <fingerprint>,
  "install_id": "<string>",
  "installed_at": <number|null>
}
```
- `<fingerprint>`：
```json
{
  "platform": "darwin",
  "arch": "arm64",
  "os_version": "<string>",
  "language": "<bcp47>",
  "timezone": "<tz>",
  "screen": "<WxH>",
  "device_mid": "<string>"
}
```
- 错误分支：本轮无。
- notes 要点：空池时 `stats` 各计数为 0；`providers` 为固定枚举 `[zai, bigmodel]`；`ts` 为 epoch 秒（随环境变化）；`*_at` 为 epoch 秒或 `null`；`fingerprint` 为设备档案对象（`device_mid` 已脱敏）；public_view 字段集 = `id/name/provider/mode/token_masked/enabled/status/quota/plan/plans/use_count/fail_count/risk_strikes/recent_results/last_used_at/last_checked_at/cooling_until/last_error/created_at/fingerprint/install_id/installed_at`。

### 3. GET /admin/api/status
- 无参数；无请求体。
- 成功 200：
```json
{
  "providers": [ "zai", "bigmodel" ],
  "gateway_key_set": <bool>,
  "quota_pool": {
    "zai": <number>,
    "bigmodel": <number>
  }
}
```
- 错误分支：本轮无。
- notes 要点：`gateway_key_set` 反映网关 Key 是否配置；`quota_pool` 为各 provider 可选用账号数。

### 4. POST /admin/api/accounts
- 无路径参数。
- 请求体：
```json
{
  "name": "<string>",
  "provider": "zai",
  "tokens": [ "<string>", ... ]
}
```
  - `provider` 可枚举：`zai` | `bigmodel`（由 `providers` 数组给出）；错误样本使用 `nope`。
  - `name` 可选：成功样本带 `name`，两个错误样本不带。
  - `tokens` 为字符串数组，样本元素已脱敏为 `<redacted:string>`。
- 成功 200：
```json
{ "count": <number>, "ids": [ "<slug>-<8hex>", ... ] }
```
- 错误分支：
  - 400 `{"detail": "不支持的 provider"}` —— `provider` 不在枚举内（样本 `provider:"nope"`）。样本 `04-accounts-add-badprovider.POST.json`。
  - 400 `{"detail": "请输入至少一个 Token / API Key"}` —— `tokens` 为空数组。样本 `04-accounts-add-notokens.POST.json`。
- notes 要点：`ids` 为服务端生成的账号 id，形态 `<slug>-<8hex>`（样本已脱敏）。

### 5. DELETE /admin/api/accounts
- 无路径参数。
- 请求体：顶层 **JSON 数组**（非对象），元素为账号 id 字符串：
```json
[ "<account_id>", ... ]
```
- 成功 200：
```json
{ "deleted": <number> }
```
  - 全部 id 不存在时 `deleted` 为 0（不报错）；样本 `05-accounts-delete-none.DELETE.json`。
- 错误分支：本轮无。
- notes 要点：成功删除给定 ids；返回 `{deleted:<实际删除数>}`；重复 id / 不存在的 id 不计入 `deleted`。

### 6. PUT /admin/api/accounts/{account_id}
- 路径参数：`account_id`。
- 请求体（样本）：
```json
{ "name": "<string>" }
```
- 成功 200：
```json
{ "ok": true }
```
- 错误分支：
  - 404 `{"detail": "账号不存在"}` —— 目标账号不存在。样本 `06-account-edit-404.PUT.json`。
- notes 要点：成功编辑改 `name`，返回 `{ok:true}`；路径参数为账号 id（成功样本脱敏为 `<redacted:account-id>`）。

### 7. POST /admin/api/accounts/{account_id}/enabled
- 路径参数：`account_id`。
- 请求体：
```json
{ "enabled": <bool> }
```
- 成功 200：
```json
{ "ok": true }
```
- 错误分支：
  - 404 `{"detail": "账号不存在"}`。样本 `07-account-enabled-404.POST.json`。
- notes 要点：`enabled=false` 禁用该账号；返回 `{ok:true}`。

### 8. POST /admin/api/accounts/{account_id}/fingerprint/rotate
- 路径参数：`account_id`；无请求体。
- 成功 200：
```json
{
  "ok": true,
  "fingerprint": <fingerprint>
}
```
  - `<fingerprint>` 结构同第 2 节。
- 错误分支：
  - 404 `{"detail": "账号不存在"}`。样本 `08-account-fingerprint-404.POST.json`。
- notes 要点：换发设备档案；`device_mid` 已脱敏，其余为枚举字符串。

### 9. POST /admin/api/accounts/refresh
- 无路径参数。
- 请求体（样本）：
```json
{ "all": true }
```
- 成功 200：
```json
{
  "summary": {
    "ok": <number>,
    "fail": <number>
  },
  "count": <number>,
  "skipped_cooling": <number>,
  "skipped_invalid": <number>
}
```
- 错误分支：本轮无。
- notes 要点：`all=true`；空池 / 无 JWT 账号时 `count=0`，`summary` 为空映射（样本为 `{fail:0, ok:0}`）。

### 10. POST /admin/api/accounts/{account_id}/refresh
- 路径参数：`account_id`；无请求体。
- 成功 200（非 JWT 账号分支）：
```json
{
  "ok": false,
  "message": "仅 Coding Plan (JWT) 账号支持额度查询"
}
```
- 错误分支：
  - 404 `{"detail": "账号不存在"}`。样本 `10-account-refresh-404.POST.json`。
- notes 要点：非 JWT（apiKey）账号不查上游，返回 `{ok:false, message:…}`；真正的 JWT 成功分支样本未覆盖。

### 11. POST /admin/api/login/start
- 无路径参数。
- 请求体（样本）：
```json
{ "label": "<string>" }
```
- 成功 200：
```json
{
  "flow_id": "<32hex>",
  "authorize_url": "<url>",
  "expires_in": <number>
}
```
- 错误分支（notes 提及，**无样本**）：
  - 502 `{"detail": "登录初始化失败: …"}` —— 上游不可达。
- notes 要点：发起 Z.AI OAuth；`flow_id` 为服务端会话标识（32 位 hex，样本已脱敏）；`authorize_url` 为上游授权地址（含上游域名，非本机，样本已脱敏）；`expires_in` 样本值 300。

### 12. GET /admin/api/login/poll/{flow_id}
- 路径参数：`flow_id`；无请求体。
- 成功 200：
```json
{ "status": "expired" }
```
- 错误分支：无（未知 flow_id 也返回 200）。
- notes 要点：未知 `flow_id` 一律返回 `{status:expired}`（HTTP 200），不返回 404；已知 flow 的其它取值 `pending` / `ready` / `failed`（`failed` 附 `message`）——样本未覆盖。

### 13. GET /admin/api/claim/preview
- 无参数；无请求体。
- 成功 200（空集）：
```json
{ "preview": [] }
```
- 错误分支：本轮无。
- notes 要点：无 JWT 账号时 `preview` 为空数组；有账号时每项含 `{account_id, account_name, plans[], error, activated, activation_error}`——非空结构样本未覆盖。

### 14. POST /admin/api/claim
- 无路径参数。
- 请求体（样本为空对象）：
```json
{}
```
- 成功 200（空集）：
```json
{
  "outcomes": [],
  "summary": {
    "ok": 0,
    "fail": 0
  }
}
```
- 错误分支：本轮无。
- notes 要点：无 JWT 候选账号时如上；有账号时 `outcomes[]` 每项含 `{account_id, account_name, ok, plan_name?, grants?, message?, code?, next_at?}`——非空结构样本未覆盖。

### 15. GET /admin/api/claim/captcha-config
- 无参数；无请求体。
- 成功 200：
```json
{
  "enabled": <bool>,
  "scene_id": "<string>",
  "region": "<string>",
  "prefix": "<string>"
}
```
  - 样本值：`enabled=true`, `prefix="no8xfe"`, `region="cn"`, `scene_id="11xygtvd"`。
- 错误分支：本轮无。
- notes 要点：返回阿里验证码 SDK 初始化参数；取值来自上游验证码服务（随环境变化，可能为空串）。

### 16. POST /admin/api/claim/manual
- 无路径参数。
- 请求体：
```json
{
  "account_id": "<uuid>",
  "captcha_verify_param": "<string>"
}
```
- 成功响应：⚠ **仅错误分支，无成功样本**。
- 错误分支：
  - 404 `{"detail": "JWT 账号不存在"}` —— 账号非 JWT 或不存在。样本 `16-claim-manual-404.POST.json`。
  - 400 `{"detail": "缺少 account_id"}` —— 请求体缺 `account_id`。样本 `16-claim-manual-missing-id.POST.json`。

### 17. GET /admin/api/settings
- 无参数；无请求体。
- 成功 200：
```json
{
  "admin_key_set": <bool>,
  "admin_key_masked": "<masked>",
  "admin_key_is_default": <bool>,
  "gateway_key_set": <bool>,
  "gateway_key_masked": "<masked>",
  "quota_refresh_interval": <number>,
  "account_concurrency": <number>,
  "claim_round_interval": <number>
}
```
  - 样本值：`account_concurrency=2`, `admin_key_is_default=true`, `admin_key_set=true`, `claim_round_interval=0`, `gateway_key_set=false`, `quota_refresh_interval=1800`。
- 错误分支：本轮无。
- notes 要点：掩码字段形如 `ab12…`（样本已脱敏为占位符）；布尔与整数为结构性取值。

### 18. PUT /admin/api/settings
- 无路径参数。
- 请求体（各样本键集不同，示例合并）：
```json
{
  "admin_key": "<string>",
  "account_concurrency": <number>,
  "claim_round_interval": <number>,
  "gateway_key": "<string>",
  "quota_refresh_interval": <number>
}
```
  - 样本请求体键集：`{admin_key}` / `{account_concurrency, claim_round_interval, gateway_key, quota_refresh_interval}` / `{gateway_key}`。
- 成功 200：
```json
{ "ok": true }
```
- 错误分支：
  - 400 `{"detail": "后台密钥不能为空"}` —— `admin_key` 为空串。样本 `18-settings-put-empty-key.PUT.json`。
- notes 要点：成功写入返回 `{ok:true}`；`gateway_key` 置空串表示网关不校验。

### 19. GET /admin/api/export
- 无参数；无请求体。
- 成功 200：
```json
{
  "version": 1,
  "exported_at": <number>,
  "providers": {
    "zai": [
      { "name": "<string>", "mode": "apiKey", "secret": "<plaintext>" }
    ],
    "bigmodel": [
      { "name": "<string>", "mode": "<string>", "secret": "<plaintext>" }
    ]
  }
}
```
- 错误分支：本轮无。
- notes 要点：`providers` 为 provider → 账号条目数组的映射；条目含 `{name, mode, secret}`；`secret` 为**明文**凭据（样本已整体脱敏为占位符）；`exported_at` 为 epoch 秒。

### 20. POST /admin/api/import
- 无路径参数。
- 请求体：
```json
{
  "providers": {
    "zai": [
      { "name": "<string>", "secret": "<plaintext>" }
    ]
  }
}
```
- 成功 200：
```json
{ "count": <number> }
```
  - 样本值 `count=1`。
- 错误分支：本轮无。
- notes 要点：`secret` 为明文凭据（样本已脱敏）。

### 21. GET /admin/api/monitoring
- 无参数；无请求体。
- 成功 200：
```json
{
  "entries": [],
  "keep": <number>
}
```
  - 样本值 `keep=500`。
- 错误分支：本轮无。
- notes 要点：`entries` 为内存环形日志，含本会话已发生的网关请求（`id/model/stream/prompt/status/…`）；重启清零；`entries` 条数随环境变化。

### 22. POST /admin/api/monitoring/clear
- 无参数；无请求体。
- 成功 200：
```json
{ "ok": true }
```
- 错误分支：本轮无。
- notes 要点：清空内存环形日志，返回 `{ok:true}`。

## 第三部分：交叉核对

### 状态码汇总
- `200`：成功。admin 全部成功样本；gateway `/v1/models` 成功。
- `400`：参数错误。accounts POST（不支持 provider / tokens 为空）、settings PUT（空 admin_key）、claim/manual（缺 account_id）、gateway 非法 JSON。
- `401`：未授权。admin verify（缺凭证）、gateway 缺 API Key。
- `403`：凭证错误。gateway `/v1/models`（错误 Key）。
- `404`：资源不存在。admin（账号不存在 / JWT 账号不存在）。
- `503`：网关无可用账号。gateway `/v1/messages`、`/v1/chat/completions`。
- `502`：仅在 `11-login-start` 的 notes 提及（登录初始化失败），**无样本**。

### 错误体形态
- `{"detail": "<string>"}`：admin **全部**错误分支（400/401/404）；gateway 的 401/403 亦为此形态。
- `{"error": {"message": "<string>", "type": "<string>"}}`：仅出现在 gateway，用于 400（非法 JSON）与 503（无可用账号）。
  - `error.type` 取值：`invalid_request`（`/v1/messages`）、`invalid_request_error`（`/v1/chat/completions`）、`no_available_account`（二者共用）。
- 结论：**并非统一形态**。admin 域统一为 `{"detail": …}`；`{"error": {…}}` 仅存在于 gateway。两种形态并存。

### 未覆盖的分支
- `claim/manual`：**无成功样本**（仅 404/400）。
- `login/start`：502 上游失败分支未采（仅 notes）。
- `login/poll`：仅 `expired`；`pending`/`ready`/`failed` 未采。
- `claim/preview`、`claim`：仅空集合；非空条目结构未采（仅 notes 文字描述）。
- `account refresh`：仅非 JWT 分支；JWT 成功分支未采。
- `accounts/refresh`：仅 `count=0`；有账号时的 `summary`/`skipped_*` 计数未采。
- `monitoring`：`entries` 为空，条目结构未采（仅 notes 描述）。
- `accounts` GET：仅一条账号样本；`plan`/`quota` 均为 `{}`、`plans`/`recent_results` 均为 `[]`，非空结构未采。
- 未采任何公开路由（无 `/meta` 类样本），无法判断哪些 admin 路由可匿名访问。
- admin 侧未采 403/429 等其它鉴权/限流分支；除 verify 外均无未授权样本。

### 不一致 / 可疑之处（共 12 条）
1. **样本数量与任务书不符**：任务书称 admin「43 条样本」，实际为 34 个 JSON。
2. ~~**键顺序疑为工具重排**~~ **【已解决 · 2026-10-03】**：初稿所见「一律字典序」确系采样器缺陷 ——
   `tools/samplecontract` 把响应体 `json.Unmarshal` 到 `map[string]any`，Go 编码 map 时按字典序输出，
   原始键顺序被抹掉。已改为**流式 Token 按原序清洗**（`sanitizeOrdered`）并重新采样全部 43 条样本。
   现在样本里的键顺序即上游真实顺序，且与 A2 的原始响应体夹具（`docs/contract/store/fixtures/`，
   从未经过 Go 编码）**完全吻合**（例如 `/admin/api/accounts` 顶层为 `accounts, stats, providers, ts`，
   账号视图为 `id, name, provider, mode, token_masked, …`）。**教训：契约采样器必须保序，否则样本不再是「真实响应体」。**
3. **账号 id 形态不一致**：`04-...-ok` 的 notes 称 id 形如 `<slug>-<8hex>`，DELETE 成功样本用 `<redacted:account-id>`；而 DELETE「none」样本 body 用 `<redacted:uuid>`，各 404 样本路径用字面 `00000000-0000-0000-0000-000000000000`。同一标识出现三种表示。
4. **脱敏规则不一致**：404 样本路径使用未脱敏的字面零 UUID，而成功样本路径使用 `<redacted:account-id>` 占位符。
5. **失败以 200 承载**：`10-account-refresh-nonjwt` 为 HTTP 200 但业务 `ok:false`；不能仅凭状态码判断成败。
6. ~~**notes 字段顺序与响应体不符**~~ **【已解决 · 2026-10-03】**：初稿时 body 是字典序、而 notes 是人工按真实顺序写的，
   两者自然对不上。重采样后 body 已恢复真实顺序，**与 notes 描述一致**（`17-settings-get` 的 body 现为
   `admin_key_set, admin_key_masked, admin_key_is_default, …`，与 notes 逐字吻合）。
7. **notes 描述了样本不可观察的结构**：`21-monitoring` 的 notes 称 `entries` 条目含 `id/model/stream/prompt/status/…`，但样本 `entries` 为空，该结构无法验证。
8. **notes 引用了无样本的错误分支**：`11-login-start` 提到 502 错误体 `{detail:登录初始化失败: …}`，`…` 的具体文本不可知。
9. **`settings` 语义存疑**：`admin_key_set=true` 与 `admin_key_is_default=true` 同时成立（可能指正在使用默认密钥），两字段关系未在样本中解释。
   *（A2 误答：`admin_key_is_default` = `admin_key == "1234"`。**A3 更正**：比的是
   **本进程配置给的默认密码**（`ZCODE_ADMIN_KEY`，未设时回落 `zcode`）—— `1234` 只是
   采样机 `.env` 的取值。三组对照实验见 `store/observations.md` §3.1。）*
10. **`settings` 写入对状态的影响无法交叉验证**：`18-settings-put-ok` 设置 `gateway_key` 后 `reset-gwkey` 又置空；`03-status` 的 `gateway_key_set=false` 与 `17-settings-get` 一致，但均为同一时点，无法验证写入后的即时反映。
11. **fingerprint 常量与变量无法区分**：`08-...-ok` 与 `02-accounts-one` 的 `platform`(darwin)、`arch`(arm64)、`screen`(1728x1117) 完全相同，而 `language`/`timezone`/`os_version` 不同（de-DE/Europe/Berlin/25.5.0 vs zh-CN/Asia/Shanghai/23.6.0）；哪些字段恒定、哪些随机，样本不足以判定。
   *（A2 已解：全部字段从观测池随机取值，仅 `platform` 与 `os_version` 强相关 —— darwin ⇒ `2x.y.z`、win32 ⇒ `10.0.x`；见 `store/fingerprint-shape.json`。）*
12. **请求体是否「部分更新」未明示**：`04-accounts-add` 成功样本带 `name`、错误样本不带；`18-settings-put` 三个样本键集各异；是否支持 patch 语义（缺省键保持原值）在样本中无直接证据。
   *（A2 已解：settings 是部分写，只更新请求里出现的键；见 `store/observations.md` #11。）*

## 第四部分：实现提示

### 随环境变化（实现时动态生成 / 计算）
- 时间类：`ts`、`created_at`、`exported_at`、`cooling_until`、`installed_at`、`last_checked_at`、`last_used_at`（epoch 秒，浮点或 `null`）。
- 标识/会话类：账号 `id`、`flow_id`（32 位 hex）、`install_id`、`fingerprint.device_mid`、`token_masked` 及各类 `*_masked` 掩码、`authorize_url`。
- 统计/计数类：`stats.*`、`quota_pool.*`、`count`、`deleted`、`skipped_cooling`、`skipped_invalid`、`summary.*`、`entries`（条数）。

### 枚举 / 固定集合（样本可见）
- `providers` = `["zai","bigmodel"]`（固定两元素、固定顺序）。
- `provider` 取值 `zai` | `bigmodel`；`mode` 样本仅见 `apiKey`（JWT 未采，勿臆造其它值）。
- `status` 样本仅见 `active`；`stats` 键名暗示还存在 `cooling`/`disabled`/`exhausted`/`invalid` 等状态，但样本未覆盖其字面值。
- `type`（gateway models）= `model`；`object`（gateway models）= `list`。
- captcha `region` 样本为 `cn`；`export.version` = 1。
- 默认配置类（样本值，是否恒定未知）：`keep=500`、`expires_in=300`、`quota_refresh_interval=1800`、`account_concurrency=2`、`claim_round_interval=0`。
  - ⚠️ `claim_round_interval=0` **是采样机 `.env` 的取值，不是代码默认值**：A3 实测不设该环境变量时首启为 **3600**（见 `store/observations.md` §3.2）。
  - ⚠️ `17-settings-get` 的 `admin_key_is_default=true` 同理依赖采样配置（`ZCODE_ADMIN_KEY=1234`），见上表第 9 条。

### Go 实现注意点
- **键顺序**：样本里的键顺序**就是上游的真实顺序**（2026-10-03 重采样后已恢复；见文件头的重采样说明）。
  Go struct 按**字段声明序**序列化，所以**按样本顺序声明字段**即可得到与上游同形的响应体。
  注意 `/admin/api/accounts` 顶层是 `accounts, stats, providers, ts`（**不是**字典序），
  账号视图是 `id, name, provider, mode, token_masked, enabled, status, …`。
  序列化时必须走 `models.MarshalNoHTMLEscape`（不转义 HTML 字符），否则含 `<` 的字段会不同形。
- **`null` vs 缺失**：`cooling_until`/`installed_at`/`last_checked_at`/`last_used_at`/`last_error` 在样本中显式为 `null`，应序列化为 `null` 而非省略（用指针或 `sql.Null*`）。
- **空容器 vs `null`**：`plan`/`quota` 为 `{}`，`plans`/`recent_results`/`preview`/`outcomes`/`entries` 为 `[]`；空容器不得写成 `null`。
- **DELETE 请求体为顶层数组**，非对象，需单独解析为 `[]string`。
- **业务失败用 200**：`ok:false` 走成功状态码，不能只看 HTTP 状态。
- **错误体分两套**：admin 用 `{"detail": …}`，gateway 用 `{"error":{"message","type"}}`，序列化时不可混用。
