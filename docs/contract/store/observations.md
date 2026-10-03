# A2 存储层契约（账号池与设置）

> **性质**：本目录记录的是靶机**可观测产出**，不是上游源码的摘抄。
> 方法：在**独立临时数据目录**里启动靶机，通过它自己的 HTTP 管理 API 造数据，
> 然后直接读它写出的 `accounts.db`（表结构 / 原始行 / 原始 `data` 字符串）。
> 规则（slug、掩码、强转）则通过**逐项变化的请求 + 回读响应**归纳。
>
> 与 A1 的关系：A1 固化的是 **HTTP 契约**（25 路由）；A2 固化的是 **落盘契约**
> —— 因为 Go 实现要接管用户**现有**的 `accounts.db`，这是风险登记 #6（账号丢失）的唯一防线。
>
> **A3 修订（2026-10-04）**：第 3 节表里的 **#8** 与 **#12** 被推翻并改正 ——
> A2 把采样机 `.env` 的取值（`admin_key=1234`、`claim_round_interval=0`）当成了
> 「代码默认值」。A3 用「移开 `.env` + 换环境变量重启」的对照实验重测，
> 结论与完整证据见 **§3.1 / §3.2**。改这两条不是文档润色：照错的写，
> `claim_round_interval` 的首启值会差 3600，`admin_key_is_default` 会恒为 false。

## 1. 落盘结构

见 `schema.sql`。两张表：`accounts`（账号行 + `data` JSON 列）、`meta`（键值对，value 一律字符串）。

`accounts` 里 `created_at` / `enabled` 与 `data` 内同名/同义字段**重复存放**，两者必须一致。

## 2. `data` 列的 JSON 形态

见 `account-data-shape.json`。**字段顺序是稳定的**（靶机用 Python `json.dumps` 输出，
保持字典插入序），实测顺序为：

```
id, name, provider, mode, jwt_token, api_key, enabled, status, quota, plan, plans,
usage, use_count, fail_count, risk_strikes, last_risk_at, recent_results,
last_used_at, last_checked_at, cooling_until, last_error, created_at,
fingerprint, install_id, installed_at
```

⚠️ **Go 侧必须按此顺序声明结构体字段**，否则落盘 blob 与靶机产出不同形。
（JSON 对象语义上无序，但这是**文件字节**层面的可观测事实，且是双向读校验的判据。）

`fingerprint` 内层顺序：`platform, arch, os_version, language, timezone, screen, device_mid`。

## 3. 归纳出的规则（逐条附证据）

| # | 规则 | 证据 |
|---|---|---|
| 1 | **provider 枚举只有 `zai` / `bigmodel`**，大小写敏感 | `p=zhipu/glm/anthropic/openai/gemini/qwen/deepseek/ZAI/BigModel/z.ai` 全 400 `不支持的 provider` |
| 2 | **账号 id = `<slug>-<8hex>`** | 见 #3 全部样本；8 位为小写 hex |
| 3 | **slug 规则**：`name` 转小写 → 逐字符「是字母数字则保留、否则替换为 `-`」→ 合并连续 `-` → 去首尾 `-` → **截断 32 字符** | `My Account!`→`my-account`；`UPPER Case`→`upper-case`；`with_under`→`with-under`；`dot.name`→`dot-name`；`a/b\c`→`a-b-c`；`t@b#c$d`→`t-b-c-d`；`  spaced  `→`spaced`；`a`×80→`a`×32；**`账号测试`→`账号测试`、`Ünïcödé`→`ünïcödé`（Unicode 字母保留 ⇒ 判据是「字母数字」而非 ASCII 白名单）** |
| 4 | **`name` 为空时默认名 = `<provider>-<该 provider 现有账号数 + 1>`** | 库内已有 2 个 bigmodel 时新增无名 zai → `zai-1`；再来一个 → `zai-2`（**按 provider 计数，不是全库计数**） |
| 5 | **去重键 = (provider, token)**；命中则返回既有 id，**且不更新 name** | 同 provider 同 token 二次新增 → 同一 id、name 仍为首次值；跨 provider 同 token → 两条独立账号 |
| 6 | **`token_masked` 规则**：`len ≤ 16` 原样返回；否则 `前 8 + '…' + 后 6` | len 4/8/12/15/16 原样；len 17→`abcdefgh…bcdefg`；len 20→`abcdefgh…efghij`；len 64→`abcdefgh…ijabcd` |
| 7 | **密钥掩码规则**（`admin_key_masked` / `gateway_key_masked`）：空串 → `""`；`len ≤ 8` → `••••`；否则 `前 4 + '…' + 后 4` | admin_key len 7/8→`••••`、len 9→`abcd…fghi`；gateway_key `''`→`""`、len 2/4/8→`••••`、len 9→`abcd…fghi` |
| 8 | **`admin_key_is_default` =（库里的 `admin_key` == 本进程配置给的默认密码）**。配置来自 `ZCODE_ADMIN_KEY`，**未配置时回落 `zcode`**；比较基准**既不是字面量、也不是库里的初值** | A3 三组对照实验，见 §3.1。**A2 原记「== `"1234"`」是错的** —— `1234` 只是采样机 `.env` 的取值 |
| 9 | **`admin_key` 不允许置空** | `PUT {"admin_key": ""}` → 400 `后台密钥不能为空`（且不改动原值） |
| 10 | **设置值强转**：接受 int / float / bool（`true`→1、`false`→0）；float **向零截断**；**负数钳到 0**；无上界；`string` / `null` → **400** | `-5`→0；`3.7`→3；`1.9`→1；`True`→1；`10^12` 原样；`"abc"`→400；`null`→400 |
| 11 | **设置「部分写」**：只更新请求里出现的键，未出现的保持不变 | 只写 `account_concurrency=7` 后，`quota_refresh_interval` 仍为 1800 |
| 12 | **首启默认值**（环境变量一个都不给时）：`admin_key="zcode"`、`gateway_key=""`、`quota_refresh_interval=1800`、`account_concurrency=2`、**`claim_round_interval=3600`**；且 `admin_key` / 三个整数都能被**同名 `ZCODE_*` 环境变量**改成别的首启值 | A3 实测，见 §3.2。**A2 原记 `claim_round_interval=0`、`admin_key="1234"` 都是这份部署 `.env` 的取值，不是默认值** |
| 13 | **`enabled` 与 `status` 联动**：`enabled=false` ⇒ `status="disabled"`；`enabled=true` ⇒ `status="active"` | 反复切换 3 次，状态每次都跟着走 |
| 14 | **新增接口固定产出 `mode="apiKey"`**，请求里塞 `mode` / `jwt_token` / `api_key` 一律被忽略 | 四种额外入参，落库后 `mode` 恒为 `apiKey`，凭据来自 `tokens[]` |
| 15 | **列表顺序 = `created_at` 升序**（即插入序） | 依次插入 zeta / alpha / mid，返回同序，`created_at` 递增 |
| 16 | **`stats` 字段与顺序**：`total, active, exhausted, cooling, invalid, disabled, calls, fail` | 逐字回读 |
| 17 | **`providers` 字段顺序**：`zai, bigmodel`（枚举声明序，非字典序） | 逐字回读 |
| 18 | **`fingerprint` 取值池**（400 次换发采样） | 见 `fingerprint-shape.json`；`platform` 与 `os_version` 强相关：darwin ⇒ `2x.y.z`，win32 ⇒ `10.0.x` |
| 19 | **建表 / 建索引必须带 `IF NOT EXISTS`**：靶机能在**已有库**上重复启动。`sqlite_master` 里存的文本**不含**该子句，因为 SQLite 会把它规范化掉 —— 所以「存文本没有它」不代表上游没用它 | 夹具 `target.db` 的 `sqlite_master`；SQLite 规范化规则；反例：Go 侧用裸 `CREATE TABLE` 时第二次启动报 `table accounts already exists`（A2 验收首条不过） |
| 20 | **库里有两个索引**：`idx_acc_provider(provider)`、`idx_acc_status(status)`（后者与 `ON` 之间是三个空格，为存文本原样） | 夹具 `target.db` 的 `sqlite_master`（共 4 个对象 + 2 个自动索引） |
| 21 | **落盘不转义 HTML 字符、不转义非 ASCII**（等价 Python `json.dumps(..., ensure_ascii=False)`） | 夹具 `data` 里 `账号测试` 是**字面 UTF-8**（不是 `\u8d26…`）。反例：Go `json.Marshal` 会把 `<` 写成 `\u003c`，**即使该类型的 `MarshalJSON` 没转义**（外层还会再压一遍）⇒ 必须走 `models.MarshalNoHTMLEscape` |
| 22 | **浮点格式 = 最短往返表示**，与 Python `repr(float)` 一致 | 夹具 `data` 与 `target-accounts.json` 紧凑化后**逐字节相同**（含 `1791041543.66092` 这类值） |
| 23 | **`meta` 的默认项是 `INSERT OR IGNORE` 语义**：已有值一律不动 | 靶机「密码首启写库、之后以库为准」；库里的 `admin_key` / 三个整数项在换一组环境变量重启后**原样保留**（§3.1 ②、§3.2） |
| 24 | **网关 Key 没有环境变量**：`ZCODE_GATEWAY_KEY` 在 `os.environ`、`.env`、以及运行期鉴权三条路径上**都不生效** | §3.2 ③。所以 `gateway_key` 的初值恒为 `""`，只能经 `PUT /admin/api/settings` 设置 |

### 3.1 `admin_key_is_default` 的比较基准（A3 对照实验）

A2 把这条记成「`admin_key == "1234"`」，理由是「置为 `1234` → true、任何其它值 → false」。
A3 用**同一数据目录 + 换环境变量重启**把它拆开了 —— 三组实验：

| # | 实验 | 结果 | 推出什么 |
|---|---|---|---|
| ① | 全新目录，`ZCODE_ADMIN_KEY=9999` | `admin_key=9999` → `is_default=true`；`PUT admin_key=1234` → **false**；`PUT admin_key=9999` → **true** | 比的是**配置值**，不是字面量 `1234` |
| ② | 库（初建于 `7777`）以 `ZCODE_ADMIN_KEY=8888` 重启 | 用 `8888` 请求 → **401**；用 `7777` 请求 → 200 但 **`is_default=false`** | 两件事：**库为准**（env 不覆盖已有值）+ 基准是**本进程的配置**而非库里的初值 |
| ③ | 承接 ②：`PUT admin_key=8888` 后再以 `ZCODE_ADMIN_KEY=8888` 重启 | `is_default=**true**` | 复核 ② 的结论：库值 == 本进程配置 ⇒ 默认 |

⇒ 完整模型：**首启时**把「配置派生值」`INSERT OR IGNORE` 进 `meta`；**之后**以库为准；
而 `is_default` 每次都拿**当前进程的配置值**与库里的值比。

### 3.2 设置项的「环境变量 → 首启默认值」链路（A3 补采）

上游 `serve` **没有任何命令行参数**（`cli.py serve --help` 会直接去启动服务），
配置全走环境变量 / `.env`。逐项实测（每次都用**全新数据目录**，并把 `.env` 移开以排除干扰）：

| 环境变量 | 取值 | 首启落库结果 | 结论 |
|---|---|---|---|
| （全不给） | — | `admin_key="zcode"`、`1800`、`2`、**`3600`** | 代码回落值 |
| `ZCODE_ADMIN_KEY` | `k1` | `admin_key="k1"`，`is_default=true` | 生效（首启默认值） |
| `ZCODE_QUOTA_REFRESH_INTERVAL` | `111` | `quota_refresh_interval=111` | 生效 |
| `ZCODE_ACCOUNT_CONCURRENCY` | `7` | `account_concurrency=7` | 生效 |
| `ZCODE_CLAIM_ROUND_INTERVAL` | `222` | `claim_round_interval=222` | 生效（`0` 也照样生效，不是「0=未配置」） |
| `ZCODE_GATEWAY_KEY` | `gw-abcdef123456` | `gateway_key_set=**false**` | **不生效** |

③ `ZCODE_GATEWAY_KEY` 的三条独立探测（都为否，故判定「无此环境变量」）：
1. 进程环境里设 `ZCODE_GATEWAY_KEY=…` + 全新目录 → `gateway_key_set=false`；
2. 把 `ZCODE_GATEWAY_KEY=…` 写进 `.env` + 全新目录 → `gateway_key_set=false`；
3. 同一实例上 `GET /v1/models` **不带** `Authorization` → **200**（若运行期也认这个变量，
   这里应当 401）。另试 `ZCODE_API_KEY` / `ZCODE_GATEWAY_TOKEN` / `ZCODE_SERVER_KEY` 亦均无效果。

⚠️ 复现这类实验时**必须把 `.env` 移开**：采样机的 `.env` 里有
`ZCODE_ADMIN_KEY=1234` 与 `ZCODE_CLAIM_ROUND_INTERVAL=0`，不排除就会把
「这份部署的取值」误当成「代码默认值」—— A2 的两条错记正是这么来的。


## 4. 本轮**未能**覆盖的分支（实现时不得凭猜测补全）

| 分支 | 原因 | 处理 |
|---|---|---|
| `mode="jwt"` 的账号 | 只能由 OAuth 登录链路产生（A5） | 字段已建模并保留 `jwt_token`；行为待 A5 补采 |
| `status` 取 `cooling` / `exhausted` / `invalid` | 需要真实账号真实调用失败才会进入 | 枚举已按 `stats` 的键名登记；**转移条件待 A4 补采** |
| `quota` / `plan` / `plans` / `usage` / `recent_results` 的**内部结构** | 空账号一律 `{}` / `[]` | **Go 侧按不透明 JSON 原样透传**，不解析、不重排，避免破坏数据 |
| `installed_at` 何时被写入 | 空账号为 `null` | 保持 `null` 透传 |
| 指纹各字段的**联合分布** | 只能观察到边际池与平台相关性 | Go 生成器从观测池取值并保证平台一致性；**随机量本就不可能逐字节复现**，验收只要求「读取既有账号逐字段一致」 |

## 5. 复现方式

```bash
# 1) 起靶机（独立临时数据目录，绝不碰真实 data/accounts.db）
cd D:/AiWork/ZCode/zcode2api
ZCODE_DATA_DIR='D:\tmp\zcode_a2_probe' ZCODE_ADMIN_KEY=1234 ZCODE_PORT=13001 \
  ./.venv/Scripts/python.exe cli.py serve

# 2) 造数据（Bearer 1234），例如
curl -s --noproxy '*' -X POST http://127.0.0.1:13001/admin/api/accounts \
  -H 'Authorization: Bearer 1234' -H 'Content-Type: application/json' \
  -d '{"provider":"zai","tokens":["probe-token-aaa"],"name":"My Account!"}'

# 3) 直接读它写出的库
python -c "import sqlite3;c=sqlite3.connect(r'D:\tmp\zcode_a2_probe\accounts.db');\
print([r for r in c.execute('select id,name,data from accounts')])"
```

采样时间：2026-10-03。靶机版本：`dengyie/zcode2api` v2.6.8（前端 2.6.3）。

## 6. 夹具（`fixtures/`）—— A2 验收的输入

上面第 3 节是**规则**，夹具是**可复现的判据**：一份靶机自己写出的库 + 它的原始响应体。
它们由 `tools/samplefixture` 在**独立临时数据目录**里驱动真实靶机生成，一次产出、入库固定：

```bash
go run ./tools/samplefixture -zcode D:/AiWork/ZCode/zcode2api -out docs/contract/store/fixtures
```

| 文件 | 内容 |
|---|---|
| `target.db` | 靶机写出的 `accounts.db`（36864 B）。6 条账号，覆盖 slug（含 Unicode）/ 默认名 / 同 provider 去重 / 跨 provider / 禁用 |
| `target-accounts.json` | `GET /admin/api/accounts` 原始响应体 |
| `target-settings.json` | `GET /admin/api/settings` 原始响应体 |
| `target-export.json` | `GET /admin/api/export` 原始响应体 |
| `target-status.json` | `GET /admin/api/status` 原始响应体 |

`target.db` **必须入库**（`.gitignore` 里对它有显式例外）—— 它是 `internal/store` 验收测试的输入。

### 验收断言 ↔ 规则 的对应

| 测试 | 断言 | 覆盖规则 |
|---|---|---|
| `TestA2AccountSnapshotRoundTrip` | 读库 → `Account` → 重新编码，键顺序 / 键集合 / 取值与原始 `data` 紧凑化后**逐字节相同** | #2–#5、#14、#21、#22、§2 |
| `TestA2ListOrderAndFields` | 列表按 `created_at` 升序；抽样字段值；去重与跨 provider 语义 | #4、#5、#14、#15 |
| `TestA2AccountsProjection` | `View()` 与 `target-accounts.json` 的 `accounts[]` 逐字段一致；`stats` 键序与取值；`providers` 枚举序 | #16、#17 |
| `TestA2SettingsView` | `settings.View()` 与 `target-settings.json` 逐字段一致 | #7、#8、#12 |
| `TestA2ReadDoesNotRewrite` | 打开 + 全量读之后，库文件 SHA256 与每行 `data` **不变** | 「读不改写」承诺 |
| `TestA2FreshSchemaMatchesTarget` | 新建库与夹具库的 `sqlite_master` 对象集合与规范化文本**完全一致**；同路径可重复打开 | #19、#20 |
| `TestInitialSettingsSeededOnFirstBoot` | 首启把五项配置初值写进 `meta`；换一组初值重开**不覆盖**；`is_default` 比的是**本进程**配置 | #8、#12、#23、§3.1 |
| `TestInitialSettingsDefaultsWithoutConfig` | 不配置时的首启值 = 常量回落值（含 `claim_round_interval=3600`） | #12、#24、§3.2 |

> 验收判据为什么是「紧凑化后比字节」而不是「直接比字节」：靶机用 Python `json.dumps`
> 的默认分隔符（`", "` / `": "`），Go 侧输出紧凑格式。**空白不属于契约**（§4 末行），
> 但**键顺序、键集合、取值、数字格式**都属于 —— 紧凑化恰好只抹掉前者。
