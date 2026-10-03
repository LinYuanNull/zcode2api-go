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

**A3 —— 管理 API**：25 个路由中的 **22 个管理路由**已实现并通过强契约测试
（43 条真实样本逐字段回放），鉴权、settings 读写、账号池 CRUD、导入导出、
监控、登录 / 领取 / 验证码的**可离线判定分支**均已就位。
网关转发（3 个路由的完整链路）、额度与验证码求解**仍是占位**，见 A4–A6。

| 阶段 | 内容 | 状态 |
|---|---|---|
| A0 | 立项与骨架（仓库结构、CI、许可、溯源） | ✅ |
| A1 | 契约固化（25 个路由的真实请求 / 响应样本） | ✅ |
| A2 | 账号池与存储（store / models / fingerprint / settings / constants） | ✅ |
| A3 | 管理 API（22 路由 + 鉴权 + settings 读写） | ✅ |
| A4 | 转发链路（调度器 / body 变换 / SSE / 错误分类） | ⬜ |
| A5 | 额度、领取、登录 | ⬜ |
| A6 | 验证码（Go 自写 CDP 客户端） | ⬜ |
| A7 | 发布 v0.1.0 | ⬜ |

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

> 上游**没有**网关 Key 的环境变量（`ZCODE_GATEWAY_KEY` 实测不生效），
> 网关 Key 的初值恒为 `""`，只能经 `PUT /admin/api/settings` 设置。
> 本实现另提供 `--admin-key` / `--gateway-key` 两个命令行开关作为便利扩展。

## 目录结构

```
zcode2api-go/
├─ cmd/zcode2api-go/          main：serve（默认）/ login / claim / set-admin-key
├─ internal/
│  ├─ store/ models/ settings/ constants/        账号池、状态机、设置、常量
│  ├─ fingerprint/ identity/ bodytransform/      设备指纹、身份头、请求体变换
│  ├─ agent/ compat/                             上游端点选择与 OpenAI 兼容层
│  ├─ quota/ claim/ oauth/ install/              额度、领取、登录、初始化
│  ├─ telemetry/ reqlog/                         遥测与请求日志
│  ├─ httpx/                                      保真 JSON 编码（`MarshalNoHTMLEscape`）与写响应
│  ├─ adminapi/ gateway/ pages/ authadmin/       管理 API、转发网关、面板、管理面鉴权
│  ├─ server/ appdir/ buildinfo/                 服务器装配、运行根解析、构建信息
│  └─ captcha/                                   验证码池与求解调度
│     └─ cdp/                                    自写极简 CDP 客户端
├─ docs/contract/             契约样本（只保留结构，见 PROVENANCE.md）
├─ tools/samplecontract/      契约采样器（开发工具，不随发布产物分发）
├─ tools/samplefixture/       落盘契约夹具生成器（开发工具）
├─ tools/spec_reorder.py      校验 SPEC.md 骨架键序与样本一致（CI 守护）
├─ PROVENANCE.md              实现依据登记（靶机 / 采样时间 / 样本编号）
└─ .github/workflows/         CI：gofmt / go vet / go build / go test / 契约键序
```

## 设计约束

- **纯标准库优先**：不引入非必要依赖；引入前先论证必要性。
- **无 CGO**：保证跨平台交叉编译与静态链接。
- **出站代理语义照实实现**：messages 走环境代理，billing / 验证码强制直连
  （否则会触发上游风控）—— 这是行为契约，不是可优化项。
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
