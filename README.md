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

**A0 —— 立项与骨架**：结构、CI、许可与溯源就位，`go build ./...` 通过；
但**所有子命令都还是骨架**，不提供任何实际功能。

| 阶段 | 内容 | 状态 |
|---|---|---|
| A0 | 立项与骨架（仓库结构、CI、许可、溯源） | ✅ |
| A1 | 契约固化（25 个路由的真实请求 / 响应样本） | ⬜ |
| A2 | 账号池与存储（store / models / fingerprint / settings / constants） | ⬜ |
| A3 | 管理 API（22 路由 + 鉴权） | ⬜ |
| A4 | 转发链路（调度器 / body 变换 / SSE / 错误分类） | ⬜ |
| A5 | 额度、领取、登录 | ⬜ |
| A6 | 验证码（Go 自写 CDP 客户端） | ⬜ |
| A7 | 发布 v0.1.0 | ⬜ |

## 快速开始

```bash
go build ./...
go run ./cmd/zcode2api-go help
```

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
│  ├─ adminapi/ gateway/ pages/ authadmin/       管理 API、转发网关、面板、管理面鉴权
│  └─ captcha/                                   验证码池与求解调度
│     └─ cdp/                                    自写极简 CDP 客户端
├─ docs/contract/             契约样本（只保留结构，见 PROVENANCE.md）
├─ PROVENANCE.md              实现依据登记（靶机 / 采样时间 / 样本编号）
└─ .github/workflows/         CI：gofmt / go vet / go build / go test
```

## 设计约束

- **纯标准库优先**：不引入非必要依赖；引入前先论证必要性。
- **无 CGO**：保证跨平台交叉编译与静态链接。
- **出站代理语义照实实现**：messages 走环境代理，billing / 验证码强制直连
  （否则会触发上游风控）—— 这是行为契约，不是可优化项。
- **错误分类必须显式**：验证码挑战 / 风控退避 / 额度耗尽 / 401 / 429 / 5xx
  各有独立处理，不合并成笼统的「上游错误」。

## 许可

MIT，见 [`LICENSE`](LICENSE)。本仓库不含任何 AGPL 代码。
