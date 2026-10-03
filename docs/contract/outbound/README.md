# 出站契约样本（网关 → 上游）

A1 采的是**入站**契约（客户端 → 网关），A2 采的是**落盘**契约，这里采的是
**出站**契约（网关 → `zcode.z.ai` / `api.z.ai`）—— A4 转发链路的实现依据。

## 为什么需要单独采样

转发只在**有可用账号**时发生。账号池为空时入站一律 `503 no_available_account`，
出站一个字节都看不到。所以 A1 的入站样本**完全没有覆盖转发链路**。

## 怎么采的（可复现）

三个实测事实拼出这条路：

1. **新增的 apiKey 账号初始 `status=active`** ⇒ 网关认为可用 ⇒ 会真的发起出站调用。
   （所以**不需要真实账号**就能采到请求侧。）
2. **出站地址硬编码**（10 个候选环境变量实测都不生效），**但出站走 `HTTPS_PROXY`**。
3. **httpx 在 `trust_env=True` 时读 `SSL_CERT_FILE`** ⇒ 设成本地自签 CA 即可解密 TLS。

于是工具链是：

```bash
# 1) 编译本地 MITM（自签 CA + zcode.z.ai / api.z.ai 叶子证书，纯标准库）
go build -o /tmp/mitmupstream.exe ./tools/mitmupstream

# 2) 起靶机（临时数据目录）+ 假账号 + 触发转发，全程经 MITM 捕获
MITM_EXE=/tmp/mitmupstream.exe python tools/capture_outbound.py \
    --target-dir D:/AiWork/ZCode/zcode2api
```

`capture_outbound.py` 负责：起 MITM → 起靶机（`HTTPS_PROXY` + `SSL_CERT_FILE` 指向 MITM）
→ 预置若干**唯一 token** 的假账号 → 逐场景打 `/v1/messages` 与 `/v1/chat/completions`
→ 把捕获记录写成 JSON Lines。

> ⚠️ **模型名必须在目录内**（如 `GLM-5.3`）；目录外模型（如 `glm-4.6`）是否出站**不稳定**。
> ⚠️ **每个场景要有独立的活跃账号**：一次失败的转发会消耗多个账号（实测 ≈4–5 个/请求）。

## 文件

| 文件 | 内容 |
|---|---|
| `01-client-configs.GET.json` | `GET zcode.z.ai/api/v1/client/configs` 的请求 + 响应形状（**含 `/v1/models` 模型表的来源**） |
| `02-messages.POST.json` | `POST api.z.ai/api/anthropic/v1/messages` 的**完整出站头 + body 变换** + 401 错误体 |
| `03-event-report.POST.json` | `POST zcode.z.ai/api/v1/event/report` 遥测形状 |
| `fixtures/client-configs.json` | 上面那条 `client/configs` 的**完整真实响应**（23,295 B，公开目录，无凭据） |
| `observations.md` | **实测钉死的规则** + 未覆盖项 |

## 许可纪律

样本是**本机运行靶机产生的网络流量记录**，不是上游源码；上游源码仍不在本仓库内。
`fixtures/client-configs.json` 是 z.ai 的**公开客户端配置**（无凭据、无用户数据），
保留它是为了让模型目录解析可离线复现。

按 [`../README.md`](../README.md) 的脱敏策略，夹具里唯一一处 32 位 hex（响应尾部的
`logid`，服务端逐次生成的追踪号）已按**取值规则**替换为 `<redacted:hex32>`；
除此之外**逐字节保持采样原样**（含键顺序）。
