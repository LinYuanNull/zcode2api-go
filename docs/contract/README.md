# 契约样本（docs/contract）

这里存放从**靶机**（本机运行的上游 `dengyie/zcode2api`）采集到的真实请求 / 响应
样本。它们是本仓库实现与验收的判据来源，也是「独立实现」主张的证据链
（见 [`../../PROVENANCE.md`](../../PROVENANCE.md)）。

## 硬规矩

1. **只保留结构。** 落盘的样本必须去掉：Cookie、`Authorization` 头、账号标识
   （邮箱 / 手机号 / 用户 id）、设备指纹原值、验证码 token 原值、以及任何可定位到
   个人的字段。这些位置一律替换成类型占位符，例如 `<redacted:string>`。
2. **错误分支同样要采。** 只采 200 会让实现者「以为没有错误路径」。每个路由至少
   覆盖：成功、参数错误、未授权、上游异常（若可达）。
3. **可复现。** 每条样本旁边要写明复现所需的输入（路由、方法、关键请求字段的**形状**
   而非原值）。
4. **不采「实现细节」。** 只采跨进程可观察的 HTTP 行为；不记录上游内部日志、函数名、
   堆栈或源码片段。
5. **保留原始键顺序（仅指响应体）。** 响应体里的键顺序是**可观测契约**的一部分（A2 已证明：
   落盘 `data` 与 `/admin/api/accounts` 的键序都不是字典序，且被逐字节校验）。采样器必须用
   **流式 Token** 清洗响应体（`tools/samplecontract` 的 `sanitizeOrdered`），**不得**解到
   `map[string]any` 再编码 —— Go 编码 map 会按字典序输出，样本就不再是「真实响应体」了。
   > 这条是踩过坑才加的：2026-10-03 前的样本键顺序全是字典序，是采样器缺陷的产物；
   > 修复后已重采样。见 `admin/SPEC.md` 文件头的重采样说明。
   >
   > **请求体不受此约束**：请求体是采样器自己构造的（Go `map` 字面量），其键顺序是工具产物。
   > 服务器不依赖请求体键顺序，所以这里不构成契约。样本里请求体呈字典序属预期现象。

## 文件命名

```
docs/contract/<area>/<NN>-<route-slug>.<method>.json
```

- `<area>`：`admin`（管理 API）/ `gateway`（网关 API）/ `claim`（领取）等。
- `<NN>`：两位序号，按采集顺序，便于对照 `PROVENANCE.md` 的记录表。
- 例：`docs/contract/admin/03-settings-get.GET.json`

## 样本结构

```json
{
  "route": "/admin/api/settings",
  "method": "GET",
  "request": {
    "query": {},
    "headers": { "Authorization": "<redacted:bearer>" },
    "body": null
  },
  "response": {
    "status": 200,
    "headers": { "Content-Type": "application/json" },
    "body": { }
  },
  "notes": "复现输入说明；哪些字段是结构性的、哪些值会随环境变化"
}
```

## 落盘契约（`store/`）

`store/` 是与上面 HTTP 样本并列的另一类契约：**落盘契约**。它记录的是靶机写出的
`accounts.db`（表结构 / 索引 / 原始 `data` 字符串）以及由它归纳出的规则 ——
因为 Go 实现要接管用户**现有**的账号库，这是「账号不丢失」的唯一防线。

| 文件 | 内容 |
|---|---|
| `store/schema.sql` | 靶机库的 `sqlite_master` 存文本（表 + 索引），以及 `IF NOT EXISTS` 规范化说明 |
| `store/account-data-shape.json` | `data` 列的 25 个字段与**固定键顺序** |
| `store/fingerprint-shape.json` | 设备指纹的取值池与平台相关性 |
| `store/observations.md` | 23 条归纳规则（逐条附证据）+ 未覆盖分支 + 夹具说明 |
| `store/fixtures/` | **可复现判据**：靶机写出的库 + 原始响应体（`go test` 的输入） |

夹具由 `tools/samplefixture/` 在独立临时数据目录里生成，**只含合成数据**
（`api_key` 形如 `fixture-token-NNNN`、`gateway_key` 为合成值），不含任何真实凭据。

## 采样工具

采样器在 `tools/samplecontract/`（Go，仅标准库）。它是**开发工具**，不随发布产物分发。
用法与脱敏规则见该目录 `main.go` 的包注释；采集记录见 [`../../PROVENANCE.md`](../../PROVENANCE.md)。

工具按两层规则脱敏：**键规则**（`token` / `secret` / `admin_key` / `device_mid` /
`flow_id` / 验证码参数 … → 类型占位符）与**取值规则**（邮箱 / 手机号 / UUID / JWT /
32 位 hex 形态的字符串 → 类型占位符）。账号 id（形态 `<slug>-<8hex>`）另做精确替换，
覆盖 `route` 字段里的路径参数。
