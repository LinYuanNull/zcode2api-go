// Package store_test 是 A2 的验收测试：**双向读校验**。
//
// 验收原文（docs/zcode-native-port-plan.md §5.2）：
//
//	「能读现有 data/accounts.db 并产出与 Python 侧**逐字段一致**的账号快照」
//
// 判据为什么是「逐字段」而不是「逐字节」：靶机用 Python `json.dumps` 落盘，
// 分隔符是 `", "` / `": "`；Go 侧是紧凑输出。两者的**键顺序、键集合、取值**必须一致，
// 但空白不属于契约（observations.md §2 与 §4 末行都写明了这一点）。
// 所以本测试的做法是：**两边都 json.Compact 之后比字节** ——
// 这同时锁住了键顺序、键集合、取值与数字格式，而不锁无意义的空白。
//
// 夹具由 `go run ./tools/samplefixture` 从**独立临时数据目录**里的真实靶机抓取，
// 不触碰用户真实 data/accounts.db。见 docs/contract/store/observations.md §5。
package store_test

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
	"github.com/LinYuanNull/zcode2api-go/internal/settings"
	"github.com/LinYuanNull/zcode2api-go/internal/store"
)

const fixtureDir = "../../docs/contract/store/fixtures"

// rawRow 是靶机库里一行的原始形态（`data` 保持落盘时的原始字符串）。
type rawRow struct {
	ID   string
	Data string
}

// readRawRows 直接读靶机库，按 created_at 升序返回原始行。
func readRawRows(t *testing.T, path string) []rawRow {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("打开夹具库失败: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, data FROM accounts ORDER BY created_at ASC, id ASC`)
	if err != nil {
		t.Fatalf("查询夹具库失败: %v", err)
	}
	defer rows.Close()
	var out []rawRow
	for rows.Next() {
		var r rawRow
		if err := rows.Scan(&r.ID, &r.Data); err != nil {
			t.Fatalf("扫描夹具行失败: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历夹具行失败: %v", err)
	}
	return out
}

// compact 去掉 JSON 里的无意义空白（不改变语义）。
func compact(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		t.Fatalf("JSON 非法: %v\n%s", err, b)
	}
	return buf.Bytes()
}

// jsonKeys 按出现顺序返回 JSON 对象的键（用于给出比字节差异更好读的失败信息）。
func jsonKeys(t *testing.T, b []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("读取 JSON 失败: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		t.Fatalf("期望 JSON 对象，实际 %v", tok)
	}
	var keys []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			t.Fatalf("读取键失败: %v", err)
		}
		keys = append(keys, kt.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("跳过值失败: %v", err)
		}
	}
	return keys
}

// openFixture 把夹具库复制到临时目录再打开 —— 夹具本身只读。
func openFixture(t *testing.T) (*store.Store, string, []byte) {
	t.Helper()
	src := filepath.Join(fixtureDir, "target.db")
	orig, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读取夹具 %s 失败: %v", src, err)
	}
	dst := filepath.Join(t.TempDir(), "accounts.db")
	if err := os.WriteFile(dst, orig, 0o644); err != nil {
		t.Fatalf("复制夹具失败: %v", err)
	}
	st, err := store.Open(filepath.ToSlash(dst))
	if err != nil {
		t.Fatalf("打开夹具库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dst, orig
}

// TestA2AccountSnapshotRoundTrip 是 A2 的核心验收：读靶机库 → 我们的 Account →
// 重新编码后必须与靶机原始 `data` **键顺序、键集合、取值**全部一致。
func TestA2AccountSnapshotRoundTrip(t *testing.T) {
	st, _, _ := openFixture(t)
	want := readRawRows(t, filepath.Join(fixtureDir, "target.db"))

	got := st.List()
	if len(got) != len(want) {
		t.Fatalf("账号数不一致: 我们的实现 %d，靶机库 %d", len(got), len(want))
	}

	for i, w := range want {
		a := got[i]
		if a.ID != w.ID {
			t.Errorf("第 %d 条顺序不一致: 我们的实现 %q，靶机 %q", i, a.ID, w.ID)
			continue
		}

		// ① 键顺序：显式断言，失败信息比字节差异直观得多。
		enc, err := models.MarshalNoHTMLEscape(a)
		if err != nil {
			t.Fatalf("账号 %s 编码失败: %v", a.ID, err)
		}
		if gk, wk := jsonKeys(t, enc), jsonKeys(t, []byte(w.Data)); !equalStrings(gk, wk) {
			t.Errorf("账号 %s 的 data 键顺序不一致:\n  我们的实现 %v\n  靶机       %v", a.ID, gk, wk)
			continue
		}

		// ② 取值 + 数字格式：两边都紧凑化后比字节。
		if gb, wb := compact(t, enc), compact(t, []byte(w.Data)); !bytes.Equal(gb, wb) {
			t.Errorf("账号 %s 的 data 不一致:\n  我们的实现 %s\n  靶机       %s", a.ID, gb, wb)
		}
	}
}

// TestA2ListOrderAndFields 锁住「列表顺序 = created_at 升序」以及读取后的列值。
func TestA2ListOrderAndFields(t *testing.T) {
	st, _, _ := openFixture(t)
	want := readRawRows(t, filepath.Join(fixtureDir, "target.db"))
	got := st.List()

	// created_at 必须严格非降序（observations.md #15）。
	for i := 1; i < len(got); i++ {
		if got[i-1].CreatedAt > got[i].CreatedAt {
			t.Errorf("列表未按 created_at 升序: %v(%v) 在 %v(%v) 之前",
				got[i-1].ID, got[i-1].CreatedAt, got[i].ID, got[i].CreatedAt)
		}
	}

	// 已知取值的抽样断言（防止「读到了但字段错位」）。
	byID := map[string]models.Account{}
	for _, a := range got {
		byID[a.ID] = a
	}
	cases := []struct {
		id       string
		name     string
		provider string
		enabled  bool
		status   string
	}{
		{"fixture-alpha-67fcd352", "fixture-renamed", "zai", true, constants.StatusActive},
		{"upper-case-66d32ec6", "UPPER Case!", "zai", true, constants.StatusActive},
		{"账号测试-028ba5e0", "账号测试", "zai", true, constants.StatusActive},
		{"zai-4-6418ca90", "zai-4", "zai", true, constants.StatusActive},
		{"bm-one-eaa000bd", "bm-one", "bigmodel", false, constants.StatusDisabled},
		{"cross-provider-548be592", "cross-provider", "bigmodel", true, constants.StatusActive},
	}
	for _, c := range cases {
		a, ok := byID[c.id]
		if !ok {
			t.Errorf("缺少账号 %s", c.id)
			continue
		}
		if a.Name != c.name || a.Provider != c.provider || a.Enabled != c.enabled || a.Status != c.status {
			t.Errorf("账号 %s 字段不符: got name=%q provider=%q enabled=%v status=%q",
				c.id, a.Name, a.Provider, a.Enabled, a.Status)
		}
		if a.Mode != constants.ModeAPIKey {
			t.Errorf("账号 %s 的 mode 应为 apiKey，实际 %q", c.id, a.Mode)
		}
	}

	// 去重语义的落盘证据：跨 provider 同 token 是两条独立账号。
	// （observations.md #5 —— 同 provider 同 token 只留一条，所以 zai 侧 token-0001 只有一条。）
	zaiTok1 := 0
	for _, a := range got {
		if a.Provider == "zai" && a.Credential() == "fixture-token-0001" {
			zaiTok1++
		}
	}
	if zaiTok1 != 1 {
		t.Errorf("zai 侧 fixture-token-0001 应只有 1 条（去重），实际 %d 条", zaiTok1)
	}
	if _, ok := byID["cross-provider-548be592"]; !ok {
		t.Error("跨 provider 同 token 应产生独立账号")
	}

	if len(want) != len(got) {
		t.Fatalf("账号数不一致: %d vs %d", len(got), len(want))
	}
}

// TestA2AccountsProjection 校验 `GET /admin/api/accounts` 的投影与靶机原始响应逐字段一致。
func TestA2AccountsProjection(t *testing.T) {
	st, _, _ := openFixture(t)
	var target struct {
		Accounts  []json.RawMessage `json:"accounts"`
		Stats     json.RawMessage   `json:"stats"`
		Providers []string          `json:"providers"`
	}
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "target-accounts.json"))
	if err != nil {
		t.Fatalf("读取 target-accounts.json 失败: %v", err)
	}
	if err := json.Unmarshal(raw, &target); err != nil {
		t.Fatalf("解析 target-accounts.json 失败: %v", err)
	}

	list := st.List()
	if len(list) != len(target.Accounts) {
		t.Fatalf("账号数不一致: 我们的实现 %d，靶机响应 %d", len(list), len(target.Accounts))
	}
	for i, a := range list {
		enc, err := models.MarshalNoHTMLEscape(a.View())
		if err != nil {
			t.Fatalf("账号 %s 投影编码失败: %v", a.ID, err)
		}
		if gb, wb := compact(t, enc), compact(t, target.Accounts[i]); !bytes.Equal(gb, wb) {
			t.Errorf("第 %d 条账号投影不一致:\n  我们的实现 %s\n  靶机       %s", i, gb, wb)
		}
	}

	// stats
	statsJSON, err := models.MarshalNoHTMLEscape(models.ComputeStats(list))
	if err != nil {
		t.Fatalf("stats 编码失败: %v", err)
	}
	if gb, wb := compact(t, statsJSON), compact(t, target.Stats); !bytes.Equal(gb, wb) {
		t.Errorf("stats 不一致:\n  我们的实现 %s\n  靶机       %s", gb, wb)
	}

	// providers（枚举声明序，非字典序）
	if !equalStrings(constants.Providers, target.Providers) {
		t.Errorf("providers 不一致: 我们的实现 %v，靶机 %v", constants.Providers, target.Providers)
	}
}

// TestA2SettingsView 校验 `GET /admin/api/settings` 的投影与靶机原始响应逐字段一致。
func TestA2SettingsView(t *testing.T) {
	st, _, _ := openFixture(t)

	s, err := settings.Load(st)
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	got, err := models.MarshalNoHTMLEscape(s.View())
	if err != nil {
		t.Fatalf("设置投影编码失败: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "target-settings.json"))
	if err != nil {
		t.Fatalf("读取 target-settings.json 失败: %v", err)
	}
	if gb, wb := compact(t, got), compact(t, raw); !bytes.Equal(gb, wb) {
		t.Errorf("设置视图不一致:\n  我们的实现 %s\n  靶机       %s", gb, wb)
	}

	// 取值层面再断一次，避免「格式对但值错」。
	if s.AdminKey != "1234" || s.GatewayKey != "fixture-gw-key-abcdef" ||
		s.QuotaRefreshInterval != 900 || s.AccountConcurrency != 3 || s.ClaimRoundInterval != 5 {
		t.Errorf("设置取值不符: %+v", s)
	}
}

// TestA2ReadDoesNotRewrite 是「读不改写」的守卫：打开 + 全量读一遍之后，
// 库文件的字节与每一行 `data` 都必须原封不动。
//
// 这条不是洁癖 —— 用户现有账号库是唯一副本（风险登记 #6），
// 「只读一次就让数据变形」是不可接受的。
func TestA2ReadDoesNotRewrite(t *testing.T) {
	st, dst, orig := openFixture(t)

	// 触发全部读路径。
	_ = st.List()
	if _, err := settings.Load(st); err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	_ = st.CountByProvider("zai")
	if _, ok := st.FindByCredential("zai", "fixture-token-0001"); !ok {
		t.Error("按凭据查找应命中 zai/fixture-token-0001")
	}

	// ① 行级：`data` 与 id 必须与夹具原始库一致。
	want := readRawRows(t, filepath.Join(fixtureDir, "target.db"))
	got := readRawRows(t, dst)
	if len(got) != len(want) {
		t.Fatalf("行数变化: %d -> %d", len(want), len(got))
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Data != want[i].Data {
			t.Errorf("第 %d 行被改写:\n  读前 id=%s data=%s\n  读后 id=%s data=%s",
				i, want[i].ID, want[i].Data, got[i].ID, got[i].Data)
		}
	}

	// ② 文件级：SQLite 在纯读路径下不应改动文件字节。
	after, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("回读库文件失败: %v", err)
	}
	if sha256.Sum256(after) != sha256.Sum256(orig) {
		t.Errorf("纯读之后库文件字节发生了变化（%d -> %d 字节）", len(orig), len(after))
	}
}

// TestA2FreshSchemaMatchesTarget 断言我们自己新建的库与靶机库**同形**：
// sqlite_master 里的对象集合，以及每个对象规范化后的 SQL 文本，必须完全一致。
//
// 这条防的是「漏建索引」这类不报错、但会让两份实现产出的库不同形的缺陷 ——
// 用户随时可能在两份实现之间来回切换，schema 必须能互相接管。
func TestA2FreshSchemaMatchesTarget(t *testing.T) {
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	st, err := store.Open(filepath.ToSlash(fresh))
	if err != nil {
		t.Fatalf("新建库失败: %v", err)
	}
	// 同一路径重复打开：靶机是 IF NOT EXISTS 语义，必须允许。
	if err := st.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	st2, err := store.Open(filepath.ToSlash(fresh))
	if err != nil {
		t.Fatalf("重复打开同一库失败（应允许）: %v", err)
	}
	_ = st2.Close()

	got := schemaObjects(t, fresh)
	want := schemaObjects(t, filepath.Join(fixtureDir, "target.db"))
	if !reflect.DeepEqual(got, want) {
		for k := range got {
			if want[k] != got[k] {
				t.Errorf("schema 对象 %s 文本不一致:\n  我们的实现 %q\n  靶机       %q", k, got[k], want[k])
			}
		}
		for k := range want {
			if _, ok := got[k]; !ok {
				t.Errorf("缺少 schema 对象 %s: %q", k, want[k])
			}
		}
		for k := range got {
			if _, ok := want[k]; !ok {
				t.Errorf("多出 schema 对象 %s: %q", k, got[k])
			}
		}
	}
}

// schemaObjects 返回库里的 schema 对象：`<type> <name>` -> 规范化 SQL（自动索引无 SQL，记为空串）。
func schemaObjects(t *testing.T, path string) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", path, err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT type, name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatalf("查询 sqlite_master 失败: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var typ, name, ddl string
		if err := rows.Scan(&typ, &name, &ddl); err != nil {
			t.Fatalf("扫描 sqlite_master 失败: %v", err)
		}
		out[typ+" "+name] = ddl
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 sqlite_master 失败: %v", err)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
