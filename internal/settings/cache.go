package settings

import "sync"

// Cache 是设置的内存快照。
//
// 为什么需要：设置被**每个请求**读（管理面鉴权要 `admin_key`、网关要
// `gateway_key`、设置页要全部）。`store` 是单写连接（`SetMaxOpenConns(1)`），
// 每次请求都回库会把单连接变成串行瓶颈 —— A4 的网关热路径尤其如此。
//
// 一致性：写入方（`PUT /admin/api/settings`）在落库后调用 `Refresh()`，
// 所以「改完立刻生效」与「库为准」两条语义都保持。外部直接改库的场景不会
// 被感知（本项目不存在这种路径）。
type Cache struct {
	mu  sync.RWMutex
	ms  MetaStore
	cur Settings
	ok  bool

	// configured 是**运行期**配置给定的设置初值，只用于
	// `admin_key_is_default` 的比较基准（见 Settings.Configured）。
	configured Configured
}

// NewCache 建一个懒加载缓存。configured 为配置给定的设置初值（可为零值）。
func NewCache(ms MetaStore, configured Configured) *Cache {
	return &Cache{ms: ms, configured: configured}
}

// Get 返回当前设置（首次调用时从存储载入）。
func (c *Cache) Get() (Settings, error) {
	c.mu.RLock()
	if c.ok {
		s := c.cur
		c.mu.RUnlock()
		return s, nil
	}
	c.mu.RUnlock()
	return c.Refresh()
}

// Refresh 强制从存储重读并更新快照。
func (c *Cache) Refresh() (Settings, error) {
	s, err := Load(c.ms, c.configured)
	if err != nil {
		return s, err
	}
	c.mu.Lock()
	c.cur = s
	c.ok = true
	c.mu.Unlock()
	return s, nil
}
