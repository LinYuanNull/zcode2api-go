// Package settings 承载网关的可写配置（靶机的 `meta` 表那五项）。
//
// 规则依据：docs/contract/store/observations.md #7–#12。
// 本包只做「取值、校验、投影」，持久化通过 MetaStore 接口注入，便于单测不碰文件。
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
)

// ErrNotANumber 表示某个设置项的值既不是数字也不是布尔。
//
// 靶机对 `string` / `null` 返回 400（observations.md #10），与「负数钳到 0」不同 ——
// 前者是拒绝，后者是接受并归一。这个区分必须保留，否则接口行为会变。
var ErrNotANumber = errors.New("设置项必须是数字")

// MetaStore 是 settings 需要的持久化能力（由 store 包实现）。
type MetaStore interface {
	GetMeta(key string) (string, bool, error)
	SetMeta(key, value string) error
}

// Settings 是五项设置的内存形态。
type Settings struct {
	AdminKey             string
	GatewayKey           string
	QuotaRefreshInterval int64
	AccountConcurrency   int64
	ClaimRoundInterval   int64
}

// Defaults 返回全新数据目录下的默认值。依据：observations.md #12。
func Defaults() Settings {
	return Settings{
		AdminKey:             constants.DefaultAdminKey,
		GatewayKey:           constants.DefaultGatewayKey,
		QuotaRefreshInterval: constants.DefaultQuotaRefreshInterval,
		AccountConcurrency:   constants.DefaultAccountConcurrency,
		ClaimRoundInterval:   constants.DefaultClaimRoundInterval,
	}
}

// Load 从存储读出设置；缺失的键回落到默认值。
func Load(ms MetaStore) (Settings, error) {
	s := Defaults()
	if v, ok, err := ms.GetMeta(constants.MetaAdminKey); err != nil {
		return s, err
	} else if ok {
		s.AdminKey = v
	}
	if v, ok, err := ms.GetMeta(constants.MetaGatewayKey); err != nil {
		return s, err
	} else if ok {
		s.GatewayKey = v
	}
	if err := loadInt(ms, constants.MetaQuotaRefreshInterval, &s.QuotaRefreshInterval); err != nil {
		return s, err
	}
	if err := loadInt(ms, constants.MetaAccountConcurrency, &s.AccountConcurrency); err != nil {
		return s, err
	}
	if err := loadInt(ms, constants.MetaClaimRoundInterval, &s.ClaimRoundInterval); err != nil {
		return s, err
	}
	return s, nil
}

func loadInt(ms MetaStore, key string, dst *int64) error {
	v, ok, err := ms.GetMeta(key)
	if err != nil || !ok {
		return err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		// 库里的值不可解析：不猜、不静默改写，直接报错交给上层决定。
		return fmt.Errorf("meta.%s 不是整数: %q", key, v)
	}
	*dst = n
	return nil
}

// Patch 是一次部分更新：nil 表示「本次不改这一项」。
//
// 用指针区分「没传」与「传了零值」—— 靶机的部分写语义正是如此
// （observations.md #11）。
type Patch struct {
	AdminKey             *string
	GatewayKey           *string
	QuotaRefreshInterval *json.RawMessage
	AccountConcurrency   *json.RawMessage
	ClaimRoundInterval   *json.RawMessage
}

// IsEmpty 报告这次更新是否什么都没改。
func (p Patch) IsEmpty() bool {
	return p.AdminKey == nil && p.GatewayKey == nil &&
		p.QuotaRefreshInterval == nil && p.AccountConcurrency == nil && p.ClaimRoundInterval == nil
}

// Apply 把 Patch 落到存储上，返回落库后的设置。
//
// 语义与靶机一致：
//   - 只写出现的键（#11）；
//   - `admin_key` 不允许为空串（#9，报 ErrAdminKeyEmpty）；
//   - 整数字段接受 int / float / bool，浮点向零截断、负数钳到 0，无上界；
//     `string` / `null` 一律 ErrNotANumber（#10）。
func Apply(ms MetaStore, p Patch) (Settings, error) {
	if p.AdminKey != nil {
		if *p.AdminKey == "" {
			return Settings{}, ErrAdminKeyEmpty
		}
		if err := ms.SetMeta(constants.MetaAdminKey, *p.AdminKey); err != nil {
			return Settings{}, err
		}
	}
	if p.GatewayKey != nil {
		if err := ms.SetMeta(constants.MetaGatewayKey, *p.GatewayKey); err != nil {
			return Settings{}, err
		}
	}
	ints := []struct {
		key string
		raw *json.RawMessage
	}{
		{constants.MetaQuotaRefreshInterval, p.QuotaRefreshInterval},
		{constants.MetaAccountConcurrency, p.AccountConcurrency},
		{constants.MetaClaimRoundInterval, p.ClaimRoundInterval},
	}
	for _, it := range ints {
		if it.raw == nil {
			continue
		}
		n, err := CoerceInt(*it.raw)
		if err != nil {
			return Settings{}, fmt.Errorf("%s: %w", it.key, err)
		}
		if err := ms.SetMeta(it.key, strconv.FormatInt(n, 10)); err != nil {
			return Settings{}, err
		}
	}
	return Load(ms)
}

// ErrAdminKeyEmpty 对应靶机的 400「后台密钥不能为空」。
var ErrAdminKeyEmpty = errors.New("后台密钥不能为空")

// CoerceInt 按靶机规则把一个 JSON 值转成整数。依据：observations.md #10。
//
// 接受：整数、浮点（**向零截断**）、布尔（true→1 / false→0）。
// 负数钳到 0；没有上界。
// 拒绝：字符串、null、以及任何其它类型。
func CoerceInt(raw json.RawMessage) (int64, error) {
	s := strings.TrimSpace(string(raw))
	switch s {
	case "", "null":
		return 0, ErrNotANumber
	case "true":
		return 1, nil
	case "false":
		return 0, nil
	}
	// 先按整数解析：避免 10^12 这种值在 float64 路径上白白过一道精度。
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return clampNonNegative(n), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, ErrNotANumber
	}
	if f > math.MaxInt64 {
		return 0, ErrNotANumber
	}
	if f < math.MinInt64 {
		return 0, ErrNotANumber
	}
	return clampNonNegative(int64(f)), nil // int64(f) 即向零截断
}

func clampNonNegative(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// View 是 `GET /admin/api/settings` 的响应体。
//
// **字段顺序为实测事实**（observations.md #7/#8），不要按字典序重排。
type View struct {
	AdminKeySet          bool   `json:"admin_key_set"`
	AdminKeyMasked       string `json:"admin_key_masked"`
	AdminKeyIsDefault    bool   `json:"admin_key_is_default"`
	GatewayKeySet        bool   `json:"gateway_key_set"`
	GatewayKeyMasked     string `json:"gateway_key_masked"`
	QuotaRefreshInterval int64  `json:"quota_refresh_interval"`
	AccountConcurrency   int64  `json:"account_concurrency"`
	ClaimRoundInterval   int64  `json:"claim_round_interval"`
}

// View 投影出对外视图。
//
// `admin_key_set` 恒为 true —— 靶机的后台密码首启即写库、且不允许置空（#9），
// 所以「未设置」这个状态在库层面不存在。
func (s Settings) View() View {
	return View{
		AdminKeySet:          s.AdminKey != "",
		AdminKeyMasked:       constants.MaskSecret(s.AdminKey),
		AdminKeyIsDefault:    s.AdminKey == constants.DefaultAdminKey,
		GatewayKeySet:        s.GatewayKey != "",
		GatewayKeyMasked:     constants.MaskSecret(s.GatewayKey),
		QuotaRefreshInterval: s.QuotaRefreshInterval,
		AccountConcurrency:   s.AccountConcurrency,
		ClaimRoundInterval:   s.ClaimRoundInterval,
	}
}
