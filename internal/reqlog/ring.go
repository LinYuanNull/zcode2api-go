// Package reqlog 请求日志：内存环形缓冲 + 落盘。
//
// A3 只落地**内存环形缓冲**（`GET /admin/api/monitoring` 与
// `POST /admin/api/monitoring/clear` 需要它）；落盘部分属于 A5。
package reqlog

import "sync"

// Entry 是一次网关请求的记录。
//
// ⚠️ **字段集未采样**：A1 的 `21-monitoring.GET.json` 里 `entries` 为空，
// 只有 notes 提到条目含 `id/model/stream/prompt/status/…`。所以这里只固化
// notes 点名的字段，外加 `ts`（日志条目的时间戳 —— 靶机其它对象一律带 `ts`，
// 这里属**推断**而非实测）。
//
// 完整的条目结构要等 A4 拿到真实网关流量后，用**非空**的 monitoring 响应补采。
// 在那之前，`…` 代表的其余字段**不猜**（见 PROVENANCE.md「A3 未覆盖的分支」）。
type Entry struct {
	ID     string  `json:"id"`
	TS     float64 `json:"ts"`
	Model  string  `json:"model"`
	Stream bool    `json:"stream"`
	Prompt string  `json:"prompt"`
	Status int     `json:"status"`
}

// Ring 是固定容量的环形缓冲。满了之后覆盖最旧的一条（与靶机的
// 「内存环形日志」语义一致，依据 SPEC.md 第 21 节）。
type Ring struct {
	mu   sync.Mutex
	buf  []Entry
	next int // 下一条要写入的下标
	full bool
	keep int
}

// New 建一个容量为 keep 的环形缓冲。keep <= 0 时回落到 1。
func New(keep int) *Ring {
	if keep <= 0 {
		keep = 1
	}
	return &Ring{buf: make([]Entry, keep), keep: keep}
}

// Keep 返回容量（`GET /admin/api/monitoring` 的 `keep` 字段）。
func (r *Ring) Keep() int { return r.keep }

// Add 追加一条记录。
func (r *Ring) Add(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = e
	r.next = (r.next + 1) % r.keep
	if r.next == 0 {
		r.full = true
	}
}

// Entries 按**从旧到新**返回全部记录（副本）。
//
// 空时返回**空切片**而不是 nil —— `json.Marshal(nil slice)` 会得到 `null`，
// 而契约要求 `[]`（SPEC.md 第四部分「空容器 vs null」）。
func (r *Ring) Entries() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, 0, r.keep)
	if !r.full {
		return append(out, r.buf[:r.next]...)
	}
	// 已绕圈：从 next 开始绕一圈即从旧到新。
	out = append(out, r.buf[r.next:]...)
	out = append(out, r.buf[:r.next]...)
	return out
}

// Len 返回当前条数。
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		return r.keep
	}
	return r.next
}

// Clear 清空（`POST /admin/api/monitoring/clear`）。
func (r *Ring) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = make([]Entry, r.keep)
	r.next = 0
	r.full = false
}
