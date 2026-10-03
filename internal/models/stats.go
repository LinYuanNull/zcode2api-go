package models

import "github.com/LinYuanNull/zcode2api-go/internal/constants"

// Stats 是 `GET /admin/api/accounts` 的 `stats` 字段。
//
// **字段顺序为实测事实**（observations.md #16），不是字典序。
//
// 注意：A1 的契约样本里这个对象是**字母序**的，那是采样器用 Go map 重新编码的产物，
// 不是靶机的顺序 —— 以本文件为准。
type Stats struct {
	Total     int64 `json:"total"`
	Active    int64 `json:"active"`
	Exhausted int64 `json:"exhausted"`
	Cooling   int64 `json:"cooling"`
	Invalid   int64 `json:"invalid"`
	Disabled  int64 `json:"disabled"`
	Calls     int64 `json:"calls"`
	Fail      int64 `json:"fail"`
}

// ComputeStats 按状态分桶统计。
//
// `active` / `exhausted` / `cooling` / `invalid` / `disabled` 是**按 status 计数**，
// `total` 是总数 —— 这几项与实测一致。
//
// ⚠️ `calls` / `fail` 的语义**尚未采样确认**（采样时全库 use_count / fail_count 均为 0，
// 两种解释都能得到 0）。这里取「求和」这一最自然的解释，并在
// docs/contract/store/observations.md 的未覆盖清单里登记，等 A4 拿到真实调用后再校准。
func ComputeStats(accounts []Account) Stats {
	var s Stats
	for _, a := range accounts {
		s.Total++
		switch a.Status {
		case constants.StatusActive:
			s.Active++
		case constants.StatusExhausted:
			s.Exhausted++
		case constants.StatusCooling:
			s.Cooling++
		case constants.StatusInvalid:
			s.Invalid++
		case constants.StatusDisabled:
			s.Disabled++
		}
		s.Calls += a.UseCount
		s.Fail += a.FailCount
	}
	return s
}
