// Package store 负责账号池的持久化：SQLite 存储 + 内存常驻快照 + 并发安全访问。
//
// 落盘结构与规则全部来自 docs/contract/store/（A2 采样），这是本项目对用户
// **现有账号数据**的兼容承诺 —— 风险登记 #6「账号丢失」的唯一防线。
//
// 设计要点：
//   - 单写连接（`SetMaxOpenConns(1)`）：SQLite 是单写者模型，串行化比调参可靠。
//   - 内存常驻快照：读路径不碰磁盘，供后续调度器高频使用。
//   - **读不改写**：List/Get 只读，绝不回写未修改的行 —— 这保证「打开一次库」
//     不会让任何既有账号的字节发生变化。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，无 CGO

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
)

// ErrNotFound 表示账号不存在。
var ErrNotFound = errors.New("账号不存在")

// 建表/建索引语句。**必须带 IF NOT EXISTS** —— 靶机（Python 侧）用的是
// `CREATE TABLE IF NOT EXISTS`，所以它能在已有库上重复启动；Go 实现要接管用户
// **现有** accounts.db，同理必须能重复打开（这是 A2 验收的第一条）。
//
// 注意 SQLite 会把 `IF NOT EXISTS` 从 sqlite_master 里**规范化掉**，
// 因此下面的语句去掉该子句后与夹具 `docs/contract/store/fixtures/target.db`
// 里存的文本逐字一致（见 store_test.go 的 schema 对齐断言）。
// 依据：docs/contract/store/schema.sql。
const (
	schemaAccounts = `CREATE TABLE IF NOT EXISTS accounts (
                    id          TEXT PRIMARY KEY,
                    provider    TEXT NOT NULL,
                    name        TEXT,
                    mode        TEXT,
                    status      TEXT,
                    enabled     INTEGER NOT NULL DEFAULT 1,
                    created_at  REAL,
                    data        TEXT NOT NULL
                )`
	schemaMeta = `CREATE TABLE IF NOT EXISTS meta (
                    key   TEXT PRIMARY KEY,
                    value TEXT NOT NULL
                )`
	// 两个索引也是靶机库里的实际对象（sqlite_master 可见）。漏掉它们会让
	// 我们自己新建的库与靶机不同形，Python 侧接管时查询计划也会不同。
	schemaIndexProvider = `CREATE INDEX IF NOT EXISTS idx_acc_provider ON accounts (provider)`
	schemaIndexStatus   = `CREATE INDEX IF NOT EXISTS idx_acc_status   ON accounts (status)`
)

var schemaDDL = []string{
	schemaAccounts,
	schemaMeta,
	schemaIndexProvider,
	schemaIndexStatus,
}

// Store 是账号池的并发安全句柄。
type Store struct {
	path string
	db   *sql.DB

	mu     sync.RWMutex
	byID   map[string]models.Account
	sorted []string // 按 created_at 升序的 id（与靶机列表顺序一致）
}

// Open 打开（不存在则创建）账号库，建表、补齐默认设置项、载入内存快照。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录失败: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开账号库失败: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{path: path, db: db, byID: map[string]models.Account{}}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.reload(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Path 返回库文件路径。
func (s *Store) Path() string { return s.path }

// Close 关闭库。
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init() error {
	for _, ddl := range schemaDDL {
		if _, err := s.db.Exec(ddl); err != nil {
			return fmt.Errorf("初始化 schema 失败: %w", err)
		}
	}
	// 补齐默认设置项。用 INSERT OR IGNORE：已有值一律不动
	// （这正是「密码首启写库、之后以库为准」的语义）。
	seed := []struct{ k, v string }{
		{constants.MetaAdminKey, constants.DefaultAdminKey},
		{constants.MetaGatewayKey, constants.DefaultGatewayKey},
		{constants.MetaQuotaRefreshInterval, itoa(constants.DefaultQuotaRefreshInterval)},
		{constants.MetaAccountConcurrency, itoa(constants.DefaultAccountConcurrency)},
		{constants.MetaClaimRoundInterval, itoa(constants.DefaultClaimRoundInterval)},
	}
	for _, it := range seed {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES(?, ?)`, it.k, it.v); err != nil {
			return fmt.Errorf("写入默认设置 %s 失败: %w", it.k, err)
		}
	}
	return nil
}

// reload 把全表读进内存快照。
func (s *Store) reload() error {
	rows, err := s.db.Query(`SELECT id, provider, name, mode, status, enabled, created_at, data FROM accounts`)
	if err != nil {
		return fmt.Errorf("读取账号失败: %w", err)
	}
	defer rows.Close()

	byID := map[string]models.Account{}
	for rows.Next() {
		var (
			id, provider, data string
			name, mode, status sql.NullString
			enabled            int64
			createdAt          sql.NullFloat64
		)
		if err := rows.Scan(&id, &provider, &name, &mode, &status, &enabled, &createdAt, &data); err != nil {
			return fmt.Errorf("扫描账号行失败: %w", err)
		}
		var a models.Account
		if err := unmarshalAccount(data, &a); err != nil {
			return fmt.Errorf("账号 %s 的 data 不是合法 JSON: %w", id, err)
		}
		// 列与 blob 重复存放：以**列**为准回填，避免两者漂移时内存态与查询语义不一致。
		a.ID = id
		a.Provider = provider
		a.Enabled = enabled != 0
		if name.Valid {
			a.Name = name.String
		}
		if mode.Valid {
			a.Mode = mode.String
		}
		if status.Valid {
			a.Status = status.String
		}
		if createdAt.Valid {
			a.CreatedAt = createdAt.Float64
		}
		byID[id] = a
	}
	if err := rows.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID = byID
	s.sorted = sortedIDs(byID)
	return nil
}

func sortedIDs(m map[string]models.Account) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	// 与靶机一致：按 created_at 升序；同一时刻用 id 兜底，保证顺序稳定可复现。
	sort.Slice(ids, func(i, j int) bool {
		a, b := m[ids[i]], m[ids[j]]
		if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt < b.CreatedAt
		}
		return ids[i] < ids[j]
	})
	return ids
}

// List 按 created_at 升序返回全部账号（副本）。
func (s *Store) List() []models.Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]models.Account, 0, len(s.sorted))
	for _, id := range s.sorted {
		out = append(out, s.byID[id])
	}
	return out
}

// Get 取单个账号。
func (s *Store) Get(id string) (models.Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.byID[id]
	return a, ok
}

// CountByProvider 返回某 provider 的账号数（生成默认显示名要用）。依据：observations.md #4。
func (s *Store) CountByProvider(provider string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, a := range s.byID {
		if a.Provider == provider {
			n++
		}
	}
	return n
}

// FindByCredential 按 (provider, 凭据明文) 找账号，用于新增时的去重。
// 依据：observations.md #5（去重键是 provider + token）。
func (s *Store) FindByCredential(provider, secret string) (models.Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.byID {
		if a.Provider == provider && a.Credential() == secret {
			return a, true
		}
	}
	return models.Account{}, false
}

// Put 插入或整体更新一个账号（同时写列与 data blob）。
func (s *Store) Put(a models.Account) error {
	blob, err := marshalAccount(a)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO accounts(id, provider, name, mode, status, enabled, created_at, data)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   provider=excluded.provider, name=excluded.name, mode=excluded.mode,
		   status=excluded.status, enabled=excluded.enabled,
		   created_at=excluded.created_at, data=excluded.data`,
		a.ID, a.Provider, a.Name, a.Mode, a.Status, boolToInt(a.Enabled), a.CreatedAt, string(blob),
	)
	if err != nil {
		return fmt.Errorf("写入账号 %s 失败: %w", a.ID, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, existed := s.byID[a.ID]; !existed {
		s.sorted = append(s.sorted, a.ID)
	}
	s.byID[a.ID] = a
	s.sorted = sortedIDs(s.byID)
	return nil
}

// Delete 删除账号。返回是否真的删掉了。
func (s *Store) Delete(id string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM accounts WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("删除账号 %s 失败: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
	s.sorted = sortedIDs(s.byID)
	return n > 0, nil
}

// GetMeta 读一个 meta 值。
func (s *Store) GetMeta(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取 meta.%s 失败: %w", key, err)
	}
	return v, true, nil
}

// SetMeta 写一个 meta 值。
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("写入 meta.%s 失败: %w", key, err)
	}
	return nil
}
