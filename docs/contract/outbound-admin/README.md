# 管理侧出站契约样本（网关管理面 → 上游）

A1 采**入站**契约（客户端 → 网关），A2 采**落盘**契约，
[`../outbound/`](../outbound/) 采**转发出站**契约（网关 → `zcode.z.ai` / `api.z.ai`，A4 用），
本目录采**管理侧出站**契约 —— **OAuth 设备码登录 / 额度查询 / 领取**三条链路，
是 A5（`internal/oauth`、`internal/quota`、claim）的实现依据。

## 为什么需要单独采

转发出站只要有一个 `apiKey` 账号就能触发（见 `../outbound/README.md`）；但
**管理侧出站需要不同前提**：

- **OAuth**：管理面本身就要发起设备码流程，**不需要任何账号**。
- **额度查询**：只有 `mode=jwt` 的账号才会真的去查 `zcode-plan/*`；
  `mode=apiKey` 的账号打 `refresh` 直接回「仅 Coding Plan (JWT) 账号支持额度查询」、**零出站**。
  所以库里塞一个 `apiKey` 账号**采不到任何东西**。

于是采样器必须在独立临时库里**注入一个「形状合法、签名无效」的假 JWT 账号**，
才能把额度查询的请求侧与「凭据失效」分支采出来。

## 怎么采的（可复现）

三个实测事实拼出这条路：

1. **假 JWT 能被当成 JWT 账号处理**：靶机按 `sub` 认账号，不看签名 ⇒ 注入后即触发额度查询。
2. **管理侧出站同样走 `HTTPS_PROXY`**（本轮实测，**更正**了「billing 强制直连」的旧记）。
3. **`httpx` 在 `trust_env=True` 时读 `SSL_CERT_FILE`** ⇒ 设成本地自签 CA 即可解密 TLS。

```bash
# 依赖：靶机在 D:/AiWork/ZCode/zcode2api（含 cli.py 与 .venv）
# MITM 由仓库内的 Go 源码现编（*.exe 不入库）：
go build -o /tmp/mitmupstream.exe ./tools/mitmupstream

# 采集并落盘（写入本目录）
MITM_EXE=/tmp/mitmupstream.exe python tools/capture_admin_outbound.py \
    --target-dir D:/AiWork/ZCode/zcode2api --emit --log-connects

# 只打印不落盘（排查用）
MITM_EXE=/tmp/mitmupstream.exe python tools/capture_admin_outbound.py \
    --target-dir D:/AiWork/ZCode/zcode2api
```

采样器分三段：**段 1**（无账号）打 OAuth `init`/`poll` 与 `claim/captcha-config`；
**段 2** 注入假 JWT 账号 → 采额度查询与 `refresh` 缓存分支 → 打 claim 三条；
**段 3** 置 `quota_refresh_interval=0` + `status=active` 重启，采「新鲜」额度分支。

> 采样全程只对本机回环端口发管理请求，用**独立临时数据目录**（`%TEMP%/zcode_a5_capture`），
> 不碰任何真实账号；控制台与落盘均按 [`observations.md`](observations.md) 第八节脱敏，
> **绝不回显凭据**。

## 文件

| 文件 | 内容 |
|---|---|
| `01-oauth-init.POST.json` | `POST zcode.z.ai/api/v1/oauth/cli/init` 的极简请求头 + gzip 响应（含 `flow_id` / `poll_token` / `authorize_url` / `expires_at` / `poll_interval_sec`） |
| `02-oauth-poll.GET.json` | `GET .../oauth/cli/poll/<flow_id>` 的请求 + `{"status":"pending"}` 响应 |
| `03-plan-usage.GET.json` | 额度查询之一：**完整指纹头** + 凭据失效时的 `404` |
| `04-plan-billing-current.GET.json` | 额度查询之二：同批并发、共用 `X-Request-Id` + `401` |
| `05-plan-billing-balance.GET.json` | 额度查询之三：同批并发、共用 `X-Request-Id` + `401` |
| `fixtures/admin-outbound-requests.json` | **本轮全部出站记录**（脱敏、按到达顺序）—— 可复现判据；`_excluded` 记录了同轮捕获但不属 A5 的 A4 噪声（`client/configs`、`event/report`） |
| `fixtures/admin-responses.json` | 管理侧（**入站**）响应旁录：本轮新出现的分支留证（`login/poll` 的 `pending`/`expired`、`accounts-add`、**`refresh` 的「新鲜」与「缓存」两种形态**、claim 三条） |
| `observations.md` | **实测钉死的规则** + 与 A4 出站头的差异 + 未覆盖项 |

## 三个必须记住的结论

1. **额度查询是「三条并发」**（`usage` + `billing/current` + `billing/balance`），
   同批**共用同一个 `X-Request-Id`**；触发点 = **新增 / 启动 / refresh**，且**只对 `active` 账号**；
   一旦转 `invalid` 则**永不再查**、`refresh` 回缓存。
2. **`refresh` 有两种形状**：真查上游走 `result`（嵌套），命中缓存走 `message`（顶层字符串）。
3. **管理侧不透明透传**：`login/start` 把上游 init 响应**重组**成
   `{flow_id, authorize_url, expires_in:300}`（丢掉 `poll_token` / `logid` / `poll_interval_sec`）。

细节与证据见 [`observations.md`](observations.md)。

## 许可纪律

样本是**本机运行靶机产生的网络流量记录**，不是上游源码；上游源码（AGPL）仍不在本仓库内。
`authorize_url` 里的 `client_id` 是**公开的 OAuth 客户端标识**（非凭据），
保留它是为了核对授权 URL 形状；其余取值均按脱敏规则替换为类型占位符。
