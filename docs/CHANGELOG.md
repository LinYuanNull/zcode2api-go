# 更新日志

本项目的版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)。

## [0.1.0] - 2026-10-04

**首个版本。** ZCode 网关的**独立纯 Go 实现** —— 一个可独立部署的本地服务，把 ZCode
上游包装成 OpenAI 兼容接口，自带账号池、管理面板与套餐定时领取。单可执行文件、
纯标准库优先、无 CGO、**纯 MIT**。

> **溯源**：本仓库是对 `dengyie/zcode2api`（Python / AGPL-3.0）的**独立 Go 重写**，
> 不是它的分支，也不含它的任何源码。上游只作为「契约采样靶机」留在开发机上，
> 用于记录真实请求 / 响应样本（采样记录见 [`PROVENANCE.md`](../PROVENANCE.md)）。

### 新增

- **管理 API（22 条路由）**：账号池 CRUD / 启停 / 刷新 / 导出导入 / 设置读写 / 监控。
  鉴权用**常数时间比较** + 按 IP 的失败锁定。上游 `frontend/` 面板可直接接本后端使用。
- **转发链路**：`/v1/messages` 保序改写出站 + 逐字节透传响应（含 SSE 逐帧 flush）；
  `/v1/chat/completions` 白名单重建出站 + 解析上游响应转成 OpenAI 形状（JSON / SSE）。
  调度器「逐个账号试到成功」，错误分类（401 / 403 / 402 / 429 / 5xx / 传输失败 / 客户端错）
  与冷却逐条对齐基线。
- **登录（OAuth 设备码）**：`login/start` 真的打上游 `oauth/cli/init`（`flow_id` 用上游给的），
  `login/poll/{flow_id}` 逐次打上游 `oauth/cli/poll` 并原样透传 `status`；
  未知 / 过期 flow 本地回 `expired`（零出站）。
- **额度查询**：`active` 的 JWT 账号**三条并发**查上游（同批共用一个 `X-Request-Id`）；
  `refresh` 的 FRESH / CACHED 两形态按**键名**区分（`result` vs `message`）；
  被判 `invalid` 之后**永不再查**。
- **领取**：`claim/preview` / `claim` / `claim/manual` / `claim/captcha-config`；
  凭据失效账号的回执与旁录样本**逐字节一致**。
- **验证码（Go 自写 CDP 客户端）**：不 import 上游那套 Node / Playwright 求解器。
  `internal/captcha/cdp` 是**手写的极简 CDP 客户端**（WebSocket 是 RFC 6455 的客户端子集，
  纯标准库），用 7 条命令驱动**系统已装的 Edge / Chrome** 跑阿里云无痕验证，
  产出 `captchaVerifyParam`（用时现解，不维护预解池）。
- **契约与证据链**：`docs/contract/` 收录 **44 条真实请求 / 响应样本**（25 条路由，
  含错误分支）；`internal/contract` 逐条重放并断言**状态码 + 响应体逐字节**（含键顺序）。
- **CLI**：`serve`（默认）/ `login` / `claim` / `captcha` / `set-admin-key` / `help`。

### 配置

上游 `serve` **没有任何命令行参数**，全部经环境变量 / `.env` 提供；本实现与之对齐，
下列变量决定**首次启动**写入数据库的初值（之后以库为准）：

| 环境变量 | 首启默认值 | 说明 |
|---|---|---|
| `ZCODE_ADMIN_KEY` | `zcode` | 管理面板后台密码 |
| `ZCODE_QUOTA_REFRESH_INTERVAL` | `1800` | 额度刷新间隔（秒） |
| `ZCODE_ACCOUNT_CONCURRENCY` | `2` | 账号并发上限 |
| `ZCODE_CLAIM_ROUND_INTERVAL` | `3600` | 定时领取轮次间隔（秒） |
| `ZCODE_CHROMIUM_PATH` | *（自动探测）* | 验证码求解用的浏览器；留空则按 Edge → Chrome 探测 |
| `ZCODE_BROWSER_HEADLESS` | `--headless=new` | 覆盖 headless 参数；置 `off` 表示不带（需要显示器） |

最后两项是**运行期**读取（改名即时生效）；另提供 `--admin-key` / `--gateway-key` /
`--data-dir` / `--panel-dir` / `--host` / `--port` 等命令行开关作为便利扩展。

### 未接通的分支（显式报错，绝不伪造）

以下分支**缺少出站样本**，一律返回 **501** 并在错误里点明原因，**不伪造成功**：

- 额度查询的 `200` 成功体结构；
- **自动领取的成功路径** —— 上游端点表里根本没有领取端点；
- OAuth `ready` 之后凭据如何落库。

三者都需要**真实账号**走完整授权才能采样，因此本版本只做到「有样本的那一半」。
把「我们还没支持」也钉成可观测行为（错误码 + 说明），而不是含糊过去。

### 已知限制

- **不随包分发浏览器**：无痕验证的判定目标就是「是不是真浏览器」，不存在又能过验证
  又小的内核；请自备 Edge / Chrome（或 `ZCODE_CHROMIUM_PATH` 指向）。
- 验证码**预解池 / `CAPTCHA_TOKEN_TTL` / `invalidate()`** 三处未实现（用时现解已足够：
  领取频率是每天一次）。
- 上游控制台面板不进本仓库（许可纪律），需自备 `--panel-dir`。

### 工程

- 单测 **230 个测试函数**，`gofmt` / `go vet` / `go test ./...` 全绿；
  CI 另跑契约键序守护（`tools/spec_reorder.py --check`）。
- 真实二进制端到端验收：管理面面板 **38/38**、额度链路 **31/31**、领取链路 **35/35**、
  验证码 **17/17**。
- `tools/mitmupstream` 本地假上游：把出站终结在回环，可**数出**出站次数并逐字节看明文，
  「claim 链路出不出站」这类事实靠它验证而非读代码相信。
