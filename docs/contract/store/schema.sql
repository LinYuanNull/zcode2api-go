-- 靶机落盘 schema（A2 采样，2026-10-03）
--
-- 来源：对靶机（dengyie/zcode2api v2.6.8）在**独立临时数据目录**里执行一遍
-- 账号增删改 / 设置读写后，直接读它自己写出的 accounts.db（`sqlite_master`）。
-- 这是「可观测产出」，不是抄上游源码；见 PROVENANCE.md「A2 采样」。
--
-- 本文件是 Go 侧 store 包的**兼容性依据**：Go 实现必须能读写同一个文件，
-- 且不改动既有账号的语义。用户现存账号不得因迁移丢失。
--
-- ⚠️ 关于 `IF NOT EXISTS`
-- 下面写的是 **sqlite_master 里存的文本**（即 SQLite 规范化之后的形态）。
-- SQLite 会把 `IF NOT EXISTS` 子句从存储文本里**去掉**，所以「存文本没有它」
-- 并不代表上游没用它 —— 恰恰相反：靶机必须能在**已有库**上重复启动，
-- 因此上游用的必然是 `CREATE TABLE IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS`。
-- Go 侧执行时必须**带上** `IF NOT EXISTS`（否则第二次启动会
-- `table accounts already exists`，A2 验收首条就不过）。

CREATE TABLE accounts (
                    id          TEXT PRIMARY KEY,
                    provider    TEXT NOT NULL,
                    name        TEXT,
                    mode        TEXT,
                    status      TEXT,
                    enabled     INTEGER NOT NULL DEFAULT 1,
                    created_at  REAL,
                    data        TEXT NOT NULL
                );

CREATE TABLE meta (
                    key   TEXT PRIMARY KEY,
                    value TEXT NOT NULL
                );

-- 两个索引也是库里的实际对象（sqlite_master 可见），不能漏建。
-- 注意 `idx_acc_status` 与 `ON` 之间是**三个空格**，这是靶机存文本的原样。
CREATE INDEX idx_acc_provider ON accounts (provider);
CREATE INDEX idx_acc_status   ON accounts (status);

-- 观察到的 meta 键（value 一律为**字符串**，即使语义是整数）：
--   admin_key              后台密码，首启写库，之后以库为准
--   gateway_key            网关 Key，未配置时为 ''
--   quota_refresh_interval 整数，默认 '1800'
--   account_concurrency    整数，默认 '2'
--   claim_round_interval   整数，默认 '0'
--
-- 注意：`accounts` 的 `created_at` 是 REAL（epoch 秒，带小数），
-- 与 `data` 里同名的 `created_at` 重复存放，两者必须一致。
-- `accounts.enabled` 是 INTEGER（0/1），与 `data.enabled`（布尔）重复。
--
-- `sqlite_autoindex_accounts_1` / `sqlite_autoindex_meta_1` 是 TEXT PRIMARY KEY
-- 自动产生的内部索引（sqlite_master 中 sql 列为 NULL），不必手工建。
