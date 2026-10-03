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

## 与上游的关系

- 本仓库是**独立实现**，不是 fork、不是分支、不是移植。
- 不随包分发上游的任何文件或二进制。
- 上游更新不会自动合入；由维护者定期比对上游 release notes 后人工决定是否跟进。
