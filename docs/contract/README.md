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

> 采样工具在 A1 阶段落地，会放在 `tools/`（不进发布产物）。
