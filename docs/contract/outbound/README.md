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
# 1) 编译本地 MITM（自签 CA + 按 CONNECT 目标动态签发叶子证书，纯标准库）
go build -o /tmp/mitmupstream.exe ./tools/mitmupstream

# 2) 起靶机（临时数据目录）+ 假账号 + 触发转发，全程经 MITM 捕获
MITM_EXE=/tmp/mitmupstream.exe python tools/capture_outbound.py \
    --target-dir D:/AiWork/ZCode/zcode2api \
    --log-connects --seed 40

# 3) 只跑个别场景做定向排查
MITM_EXE=/tmp/mitmupstream.exe python tools/capture_outbound.py \
    --target-dir D:/AiWork/ZCode/zcode2api --only m-string --log-connects
```

`capture_outbound.py` 负责：起 MITM → 起靶机（`HTTPS_PROXY` + `SSL_CERT_FILE` 指向 MITM）
→ 预置若干**唯一 token** 的假账号 → 逐场景打 `/v1/messages` 与 `/v1/chat/completions`
→ 把捕获记录写成 JSON Lines，并把**靶机自己的路由日志**（`>>>` / `[~]` / `<!>` 记号行）
和 `api.z.ai` 的请求体一并打印出来。

`--seed N` 决定预置账号数：池一空后续场景就只剩 503，所以宁多勿少（默认按场景数 ×5 估）。

> ⚠️ **模型名不影响是否出站**：目录外模型（`no-such-model-xyz`）与目录内模型都会尝试转发。
> ⚠️ **一次请求会逐个账号试到成功或池空**（失败就标记后切下一个）。假 token 必然全败 ⇒
> **一个请求就能把整个池抽干**，之后所有请求在本地直接 503、**零出站**。
> 所以「某些请求没有出站」是**池被抽干**，不是未实现的分支。
> ⚠️ **采样工具的两处缺陷已修**（未读取的 stderr 管道 / 静态证书列表）—— 详见
> `observations.md` 第六节，修复前会表现为「请求确实发出、却一条转发记录都没有」。

## 文件

| 文件 | 内容 |
|---|---|
| `01-client-configs.GET.json` | `GET zcode.z.ai/api/v1/client/configs` 的请求 + 响应形状（**含 `/v1/models` 模型表的来源**） |
| `02-messages.POST.json` | `POST api.z.ai/api/anthropic/v1/messages` 的**完整出站头 + body 变换** + 401 错误体 |
| `03-event-report.POST.json` | `POST zcode.z.ai/api/v1/event/report` 遥测形状 |
| `04-chat-completions.POST.json` | `/v1/chat/completions` 入口的出站 body —— **键序与 `/v1/messages` 不同**（重建而非保序） |
| `fixtures/client-configs.json` | 上面那条 `client/configs` 的**完整真实响应**（23,295 B，公开目录，无凭据） |
| `fixtures/outbound-requests.json` | **逐场景出站请求体**（8 条去重后的真实转发 body + 出站头）—— A4-4 body 变换的判据来源 |
| `observations.md` | **实测钉死的规则** + 工具缺陷 + 未覆盖项 |

## 许可纪律

样本是**本机运行靶机产生的网络流量记录**，不是上游源码；上游源码仍不在本仓库内。
`fixtures/client-configs.json` 是 z.ai 的**公开客户端配置**（无凭据、无用户数据），
保留它是为了让模型目录解析可离线复现。

按 [`../README.md`](../README.md) 的脱敏策略，夹具里唯一一处 32 位 hex（响应尾部的
`logid`，服务端逐次生成的追踪号）已按**取值规则**替换为 `<redacted:hex32>`；
除此之外**逐字节保持采样原样**（含键顺序）。
