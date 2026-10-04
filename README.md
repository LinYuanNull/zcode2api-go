# zcode2api-go

[![CI](https://github.com/LinYuanNull/zcode2api-go/actions/workflows/ci.yml/badge.svg)](https://github.com/LinYuanNull/zcode2api-go/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

ZCode 网关的**独立纯 Go 实现**：一个可独立部署的本地服务，把 ZCode 上游包装成
OpenAI 兼容接口，自带账号池、管理面板与套餐定时领取。纯 Go、无 Python、无 Node，
**纯 MIT**。

> **溯源**：本仓库是对 `dengyie/zcode2api`（Python / AGPL-3.0）的**独立 Go 重写**，
> 不是它的分支、不是移植副本。上游源码**不在本仓库内**，也未被复制、翻译或改写进
> 本仓库；它只作为「契约采样靶机」留在开发机上，用来记录真实请求 / 响应样本
> （见 [`PROVENANCE.md`](PROVENANCE.md) 与 [`docs/contract/`](docs/contract/)）。

## 状态

**v0.1.0 已发布** —— 版本说明见 [`docs/CHANGELOG.md`](docs/CHANGELOG.md)，
二进制见 [Releases](https://github.com/LinYuanNull/zcode2api-go/releases)。

**A4 —— 转发链路**：`/v1/messages` 与 `/v1/chat/completions` 的完整链路已就位。
`/v1/messages` 保序改写出站 + 逐字节透传响应（含 SSE 逐帧 flush）；`/v1/chat/completions`
白名单重建出站 + 解析上游响应转成 OpenAI 形状（JSON / SSE）。调度器按「逐个账号试到成功」
实现，错误分类（401/403/402/429/5xx/传输失败/客户端错）与冷却逐条对齐基线。

**A5 —— 登录 / 额度 / 领取三条管理侧链路已接通（已采样的部分全部落地）**：

- **登录**：`POST /admin/api/login/start` 会真的去打上游 `oauth/cli/init`
  （`flow_id` 用**上游**给的那个），`GET /admin/api/login/poll/{flow_id}` 逐次打上游
  `oauth/cli/poll` 并原样透传 `status`；未知 / 过期 flow 本地回 `expired`（零出站）。
- **额度**：`POST /admin/api/accounts`（主触发点）、启动自刷、`accounts/{id}/refresh`
  会对 `active` 的 JWT 账号**三条并发**查上游（同批共用一个 `X-Request-Id`）。
  `refresh` 的 FRESH / CACHED 两种形态按**键名**区分（`result` vs `message`）；
  被判 `invalid` 之后**永不再查**。
- **领取**：`claim/preview`、`claim`、`claim/manual`、`claim/captcha-config`
  **零出站**（只读已存状态）；凭据失效账号的回执与旁录样本**逐字节一致**。

**A6 —— 验证码（Go 自写 CDP 客户端）**：不 import 上游那套 Node/Playwright 求解器，
`internal/captcha/cdp` 是一个**手写的极简 CDP 客户端**（WebSocket 是 RFC 6455 的客户端子集，
纯标准库），只用 7 条命令（`Target.createTarget` → `attachToTarget` → `addScriptToEvaluateOnNewDocument`
→ `Emulation.setUserAgentOverride` → `Page.navigate` → `Runtime.evaluate` → `Target.closeTarget`）
驱动**系统已装的 Edge / Chrome** 跑阿里云无痕验证，产出 `captchaVerifyParam`。

- **不随包分发浏览器**（无痕验证的判定目标就是「是不是真浏览器」，不存在又能过验证又小的内核）：
  定位顺序 `$ZCODE_CHROMIUM_PATH` → Edge → Chrome。
- **用时现解**，不维护预解池：领取频率是每天一次，为一天一两次求解常驻几百 MB 浏览器
  不划算（预解池 / token TTL / `invalidate()` 三处已登记为未实现，见 `PROVENANCE.md`）。
- `GET /admin/api/claim/captcha-config` 的响应体与样本 `15-*` **逐字节一致**
  （**更正了 A5-4 的空配置**：空 `scene_id` 会让面板的人机验证控件整个不可用）。
- `zcode2api-go captcha [--solve] [--json]` 是自检 / 试解入口。

**未采样 ⇒ 显式报错（501），绝不伪造**：额度查询的 `200` 成功体、真实领取
（**上游端点表里根本没有领取端点**）、OAuth 的 `ready` 之后凭据落库。
三者都需要**真实账号**走完整授权，因此本实现只做到「有样本的那一半」，
并把界线写在错误里（`ErrSuccessShapeUnsampled` / `ErrUnsampled`）。
**自动领取仍未接通**：验证码求解器已能产出凭据，但「凭据往哪发」没有出站样本。

| 阶段 | 内容 | 状态 |
|---|---|---|
| A0 | 立项与骨架（仓库结构、CI、许可、溯源） | ✅ |
| A1 | 契约固化（25 个路由的真实请求 / 响应样本） | ✅ |
| A2 | 账号池与存储（store / models / fingerprint / settings / constants） | ✅ |
| A3 | 管理 API（22 路由 + 鉴权 + settings 读写） | ✅ |
| A4 | 转发链路（调度器 / body 变换 / SSE / 错误分类） | ✅ |
| A5 | 额度、领取、登录 | ✅ 登录 ✅ · 额度 ✅ · 领取 ✅（均限已采样分支） |
| A6 | 验证码（Go 自写 CDP 客户端 + 求解器 + 自检命令） | ✅（自动领取未接通） |
| A7 | 发布 v0.1.0 | ✅ |

## 下载

从 [Releases](https://github.com/LinYuanNull/zcode2api-go/releases) 取单文件可执行程序
（**不含 Chromium** —— 验证码求解复用系统已装的 Edge / Chrome）：

| 资产 | 说明 |
|---|---|
| `zcode2api-go-windows-amd64.exe` | Windows x64，无控制台窗口 |
| `zcode2api-go-linux-amd64` | Linux x64 |
| `checksums.txt` | 上面两者的 SHA256 |

```bash
# Windows
zcode2api-go-windows-amd64.exe serve
# Linux
chmod +x zcode2api-go-linux-amd64 && ./zcode2api-go-linux-amd64 serve
```

## 快速开始

```bash
go build ./...
go run ./cmd/zcode2api-go help

# 启动服务（默认子命令）；数据目录默认 exe 同级 data/
go run ./cmd/zcode2api-go serve
```

### 配置

上游 `serve` **没有任何命令行参数**，全部经环境变量 / `.env` 提供。本实现与之对齐，
下列变量决定**首次启动**写入数据库的初值（之后以库为准，改环境变量不再生效）：

| 环境变量 | 首启默认值 | 说明 |
|---|---|---|
| `ZCODE_ADMIN_KEY` | `zcode` | 管理面板后台密码；`admin_key_is_default` 即「库值 == 本进程配置值」 |
| `ZCODE_QUOTA_REFRESH_INTERVAL` | `1800` | 额度刷新间隔（秒） |
| `ZCODE_ACCOUNT_CONCURRENCY` | `2` | 账号并发上限 |
| `ZCODE_CLAIM_ROUND_INTERVAL` | `3600` | 定时领取轮次间隔（秒） |
| `ZCODE_CHROMIUM_PATH` | *（自动探测）* | 验证码求解用的浏览器；留空则按 Edge → Chrome 探测 |
| `ZCODE_BROWSER_HEADLESS` | `--headless=new` | 覆盖 headless 参数；置 `off` 表示不带（需要显示器） |

> 上游**没有**网关 Key 的环境变量（`ZCODE_GATEWAY_KEY` 实测不生效），
> 网关 Key 的初值恒为 `""`，只能经 `PUT /admin/api/settings` 设置。
> 本实现另提供 `--admin-key` / `--gateway-key` 两个命令行开关作为便利扩展。
>
> 最后两项是**运行期**读取（改名即时生效，不像上面四项只在首启写库）：
> 「验证码解不出来」第一个要查的就是 `zcode2api-go captcha` 报的浏览器路径。

## 目录结构

```
zcode2api-go/
├─ cmd/zcode2api-go/          main：serve（默认）/ login / claim / captcha / set-admin-key
├─ internal/
│  ├─ store/ models/ settings/ constants/        账号池、状态机、设置、常量
│  ├─ fingerprint/ identity/ bodytransform/      设备指纹、身份头、请求体变换
│  ├─ agent/ compat/                             上游端点选择与 OpenAI 兼容层
│  ├─ quota/ claim/ oauth/ install/              额度、领取、登录、初始化
│  ├─ telemetry/ reqlog/                         遥测与请求日志
│  ├─ httpx/                                      保真 JSON 编码（`MarshalNoHTMLEscape`）与写响应
│  ├─ adminapi/ gateway/ pages/ authadmin/       管理 API、转发网关、面板、管理面鉴权
│  ├─ server/ appdir/ buildinfo/                 服务器装配、运行根解析、构建信息
│  └─ captcha/                                   验证码配置 + 求解器（用时现解）
│     └─ cdp/                                    自写极简 CDP 客户端（WebSocket + JSON-RPC）
├─ docs/
│  ├─ contract/               契约样本（只保留结构，见 PROVENANCE.md）
│  └─ CHANGELOG.md            版本说明（Release notes 的单一真源）
├─ tools/                     开发工具（都不随发布产物分发）
│  ├─ samplecontract/         契约采样器
│  ├─ samplefixture/          落盘契约夹具生成器
│  ├─ mitmupstream/           本地 MITM / 假上游（出站逐字节对照 + 数出站次数）
│  ├─ spec_reorder.py         校验 SPEC.md 骨架键序与样本一致（CI 守护）
│  ├─ behavior_diff.py        与参考实现的行为对照
│  ├─ capture_outbound.py / capture_admin_outbound.py   出站采样（A4 / A5）
│  └─ e2e_panel.py / e2e_quota.py / e2e_claim.py / e2e_captcha.py   真实二进制端到端验收
├─ PROVENANCE.md              实现依据登记（靶机 / 采样时间 / 样本编号）
└─ .github/workflows/         CI：gofmt / go vet / go build / go test / 契约键序
```

## 设计约束

- **纯标准库优先**：不引入非必要依赖；引入前先论证必要性。
- **无 CGO**：保证跨平台交叉编译与静态链接。
- **出站代理语义照实实现**：管理侧出站（含 billing）与转发链路都读环境代理
  （`HTTPS_PROXY` / `SSL_CERT_FILE`）—— 旧记「billing / 验证码强制直连」已被实测否定，
  见 [`docs/contract/outbound-admin/observations.md`](docs/contract/outbound-admin/observations.md) 第二节。
  这是行为契约，不是可优化项（企业代理环境里必须与靶机一致）。
- **错误分类必须显式**：验证码挑战 / 风控退避 / 额度耗尽 / 401 / 429 / 5xx
  各有独立处理，不合并成笼统的「上游错误」。

## 契约保真约定（A3 实测钉死，改代码前先读）

这些不是风格偏好，而是**与上游逐字节对齐**的可观测事实，改动即破坏兼容：

- **键顺序是契约**：`docs/contract/*.json` 样本里的键顺序 = 上游真实顺序；
  Go `struct` 按**字段声明序**序列化，声明顺序即样本顺序。`tools/spec_reorder.py` 在 CI 守护。
- **保真编码唯一入口**：所有 HTTP 响应经 `httpx.WriteJSON` → `models.MarshalNoHTMLEscape`
  （不转义 `< > &`、不转义非 ASCII，对齐上游 `ensure_ascii=False`）。
- **两套错误体不可混用**：管理域统一 `{"detail": "..."}`；网关域 `{"error": {"message","type"}}`。
- **鉴权在路由匹配之后**：未知路径一律 `404 {"detail":"Not Found"}`，即便不带凭证也不返回 401。
- **业务失败用 200**：`ok:false` 走成功状态码，只有协议级错误才用 4xx/5xx。
- **空容器写 `[]` / `{}`，不写 `null`**。
- **未实现的分支一律显式报错**（501 / 502），**绝不伪造成功**。

## 许可

MIT，见 [`LICENSE`](LICENSE)。本仓库不含任何 AGPL 代码。
