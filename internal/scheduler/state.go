package scheduler

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/marks"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
)

// 本文件是**状态机落库层**：把「这次尝试意味着什么」翻译成账号行的字段变更。
// 分工上，scheduler.go 决定"算什么"，本文件只管"写什么、写成什么形态"。
//
// # 形态约束
//
// 条目形态实测（探针 B/D/E/F，库里的原始字节）：
//
//	{"ok": true, "at": 1791055416.6875126, "detail": "HTTP 200 · REQ-MODEL · 0.0s"}
//
// 键序固定 `ok, at, detail`；`at` 是带小数的 epoch 秒（Python `time.time()`），
// 不是整数秒。中文与 `·` 都**不转义**（走 `models.MarshalNoHTMLEscape`）。
//
// 至于 `", "` / `": "` 的空格：**空白不是契约**（A2 已判定，判据是「紧凑化后
// 比字节」），所以外层 `Account.MarshalJSON` 会把这里紧凑化掉。真正属于契约的
// 是键顺序、键集合、取值与数字格式。

// candidates 给出本次请求要试的账号，**按 store 的顺序**（安装序 / created_at）。
//
// 冷却中的账号在此被过滤掉：`cooling_until <= now` 时**自动恢复 active**，
// 这是探针 `probe_cooling_expiry.py` H/I 实测的（把 cooling_until 拨到过去后
// 下一次请求成功选中它、status 变回 active、cooling_until 与 last_error 双双清空）。
//
// 恢复**必须落盘**，不能只改内存视图：`mustGet` 会重新从 store 读，
// 只在内存里 revive 的话，写回时会把库里那份 `cooling` 覆盖回去
// （恢复就丢了，账号永远卡在 cooling —— 这是实测判据，也是踩过的坑）。
// 所以这里把恢复直接写库。
func (s *Scheduler) candidates() []models.Account {
	now := nowSeconds()
	out := make([]models.Account, 0, 8)
	for _, a := range s.store.List() {
		if !a.Enabled {
			continue
		}
		if a.Status == constants.StatusCooling && coolingExpired(a, now) {
			a = revive(a)
			if err := s.store.Put(a); err != nil {
				// 恢复写库失败：仍按恢复后的视图继续（本次请求不该因此失败），
				// 但要留痕 —— 否则这个账号会一直卡在 cooling。
				marks.OK(s.marks, "store", "账号 %s 冷却到期恢复写库失败：%v", a.Name, err)
			}
		}
		if a.Status != constants.StatusActive {
			continue
		}
		out = append(out, a)
	}
	return out
}

// coolingExpired 判定冷却是否到期。
//
// `cooling_until` 为 nil 时**不视为到期**：nil 的语义是"没在冷却"，但 status 已是
// cooling —— 这种组合按未到期处理更安全（宁可少试一个账号，不要拿一个状态可疑的
// 账号去打上游）。实测数据里没出现过这种组合，所以这是**明确登记的选择**而非观察。
func coolingExpired(a models.Account, now float64) bool {
	return a.CoolingUntil != nil && *a.CoolingUntil <= now
}

// revive 把到期账号恢复成 active，并清掉冷却痕迹。
func revive(a models.Account) models.Account {
	a.Status = constants.StatusActive
	a.CoolingUntil = nil
	a.LastError = nil
	return a
}

// markSuccess 记一次成功。
//
// 落库三处（实测）：`use_count += 1`、`last_used_at = now`、
// `recent_results` 追加 `{ok: true, at, detail}`。**status / last_error 不动** ——
// 能被选中的账号本来就没有冷却痕迹，成功时不需要清。
//
// detail 形态：`HTTP <code> · <模型> · <耗时>s`。两点容易写错：
//   - 模型取**入站请求体**的 `model`（不是上游回执里的模型）：探针 B 入站
//     `REQ-MODEL` ⇒ detail 写 `HTTP 200 · REQ-MODEL · 0.0s`。
//   - 耗时是**秒、保留一位小数**（`%.1f`），并带后缀 `s`；不是毫秒。
func (s *Scheduler) markSuccess(acct models.Account, req Request, code int, elapsed time.Duration) {
	now := nowSeconds()
	a := s.mustGet(acct)
	a.UseCount++
	a.LastUsedAt = &now
	model := req.Model
	if model == "" {
		model = "-"
	}
	detail := fmt.Sprintf("HTTP %d · %s · %.1fs", code, model, elapsed.Seconds())
	s.put(a, true, detail)
}

// markFailure 记一次失败（除 429 耗尽外的所有失败分类都走这里）。
//
// 落库五处：`status`、`cooling_until`、`last_error`、`last_checked_at`、
// `recent_results` 追加 `{ok: false, at, detail}`。
//
// ⚠️ `last_error` 与 `detail` **不是同一句话**，实测两者常差一个前缀：
//
//	401 → last_error "鉴权失败 HTTP 401"                    = detail
//	402 → last_error "额度已用完"                            ≠ detail "额度用完 HTTP 402"
//	5xx → last_error "上游 HTTP 500 重试 3 次耗尽，冷却"      = "上游 " + detail
//	连接 → last_error "连接失败: <原因>"                      = detail
//
// 所以本函数收两个独立参数，别图省事只传一个。
func (s *Scheduler) markFailure(acct models.Account, status, lastError, detail string) {
	now := nowSeconds()
	a := s.mustGet(acct)
	a.Status = status
	a.LastError = &lastError
	a.LastCheckedAt = &now
	if status == constants.StatusCooling {
		until := now + cooldownDuration.Seconds()
		a.CoolingUntil = &until
	}
	// 非 cooling 的失败（invalid / exhausted）**清掉 cooling_until**：
	// 实测 401/402 之后该字段是 null。
	if status != constants.StatusCooling {
		a.CoolingUntil = nil
	}
	s.put(a, false, detail)
}

// appendRecent 只追加一条 recent_results，**不动任何其它字段**。
//
// 用于 429 耗尽：实测账号 status 仍是 active、cooling_until 与 last_error 都是
// null，只有 recent_results 多了一条 `{ok: false, ..., "detail": "429 重试 5 次耗尽"}`。
// 这是"账号保持可用"这句话的落地形态。
func (s *Scheduler) appendRecent(acct models.Account, ok bool, detail string) {
	a := s.mustGet(acct)
	s.put(a, ok, detail)
}

// mustGet 取账号的**最新**视图。
//
// 关键：必须重新从 store 读，不能用调用方手里那份快照。一条请求会连续试多个账号、
// 每个账号又有多次重试，中间可能已有别的请求改了这行（探针里 2 账号 12 次出站的
// 场景每次都拿到最新值）。用旧快照写回会覆盖掉别人的变更。
func (s *Scheduler) mustGet(acct models.Account) models.Account {
	if a, ok := s.store.Get(acct.ID); ok {
		return a
	}
	return acct
}

// put 是唯一的落库出口：追加 recent_results（带 20 条上限）后写回。
func (s *Scheduler) put(a models.Account, ok bool, detail string) {
	now := nowSeconds()
	a.RecentResults = appendRecent(a.RecentResults, ok, now, detail)
	if err := s.store.Put(a); err != nil {
		// 落库失败**不能静默**：状态机丢一次会让冷却中的账号被反复选中。
		// 但也不能 panic 打挂整个网关 —— 记一行后继续。
		marks.OK(s.marks, "store", "账号 %s 状态写回失败：%v", a.Name, err)
	}
}

// appendRecent 在现有 recent_results 末尾追加一条，并把长度压到 recentKeep。
//
// 长度上限 20 是实测的（探针 F：连打 60 次成功，库里仍只保留末尾 20 条）。
// 压的是**头部**（丢最旧的），因为 recent_results 的语义是"最近的结果"。
func appendRecent(raw []byte, ok bool, at float64, detail string) []byte {
	items := decodeRecent(raw)
	items = append(items, recentEntry{Ok: ok, At: at, Detail: detail})
	if len(items) > recentKeep {
		items = items[len(items)-recentKeep:]
	}
	return encodeRecent(items)
}

// recentEntry 是 recent_results 的一项。
//
// ⚠️ **不要**给这个结构体加 `omitempty`：空字符串与 `false` 都是有效值，
// 省略它们会让 `detail: ""` 或 `ok: false` 的条目丢字段。
// 字段声明顺序 = 落盘键顺序（`ok, at, detail`），改顺序前先读
// docs/contract/store/account-data-shape.json。
type recentEntry struct {
	Ok     bool    `json:"ok"`
	At     float64 `json:"at"`
	Detail string  `json:"detail"`
}

func decodeRecent(raw []byte) []recentEntry {
	if len(raw) == 0 {
		return nil
	}
	// 走标准库解码即可：这一段是**读**，读出来的值马上会被重新编码，
	// 原始空白与缩进不参与契约（A2 已判定「空白不是契约」，见
	// docs/contract/store/observations.md 的验收判据说明）。
	var entries []recentEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	return entries
}

func encodeRecent(items []recentEntry) []byte {
	// 用 Account 落盘同一条路径（MarshalNoHTMLEscape）：不转义 HTML、
	// 数字走最短往返表示 —— 这两条都是 observations.md 里的契约项。
	b, err := models.MarshalNoHTMLEscape(items)
	if err != nil {
		// 结构体是 []recentEntry（无自定义 Marshaler），编码不会失败。
		// 真失败了返回空数组语义（调用方写回空列表）而不是崩掉整个请求。
		return []byte("[]")
	}
	return b
}

// nowSeconds 是 epoch 秒（带小数），对齐 Python 的 `time.time()`。
func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }
