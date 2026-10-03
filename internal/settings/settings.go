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

// Settings 是五项设置的内存形态，外加一组**运行期配置**（`Configured`）。
type Settings struct {
	AdminKey             string
	GatewayKey           string
	QuotaRefreshInterval int64
	AccountConcurrency   int64
	ClaimRoundInterval   int64

	// Configured 是**本次运行**配置给定的初值（环境变量 / 启动参数）。
	//
	// 它有两个用途，都来自 A3 的对照实验（见 observations.md #8/#12）：
	//  1. 首启时由 store 写进 `meta`（`INSERT OR IGNORE`），之后以库为准；
	//  2. `admin_key_is_default` 拿它作比较基准 —— 比的是**本进程配置给的值**，
	//     既不是字面量，也不是库里那份初值。
	//
	// 实测：`ZCODE_ADMIN_KEY=8888` 重启一个库里存着 `7777` 的实例 ⇒ 用 8888 被拒
	//（库为准）、用 7777 能进但 `admin_key_is_default=false`（≠ 本进程的 8888）。
	Configured Configured
}

// Configured 是本次运行的设置初值。
//
// 整数字段**原样采用**：`0` 是合法取值（实测 `ZCODE_CLAIM_ROUND_INTERVAL=0`
// 会真的落成 0），不能拿 0 当「未配置」。
type Configured struct {
	AdminKey             string
	GatewayKey           string
	QuotaRefreshInterval int64
	AccountConcurrency   int64
	ClaimRoundInterval   int64
}

// NewConfigured 归一配置初值。
//
// 只归一 `AdminKey`：空串是**非法后台密码**（#9 不允许置空），该状态无法自洽，
// 故按「未配置」处理、回落 `constants.FallbackAdminKey`。
// 实测依据：移走 `.env` 且不设 `ZCODE_ADMIN_KEY` 起靶机，`Bearer zcode` → 200。
func NewConfigured(adminKey, gatewayKey string, quotaRefresh, accountConcurrency, claimRound int64) Configured {
	return Configured{
		AdminKey:             NormalizeAdminKey(adminKey),
		GatewayKey:           gatewayKey,
		QuotaRefreshInterval: quotaRefresh,
		AccountConcurrency:   accountConcurrency,
		ClaimRoundInterval:   claimRound,
	}
}

// ConfiguredWithAdminKey 是「只配置后台密码」的简写：其余四项取 constants 回落值。
//
// 供只关心后台密码的调用方与测试使用；生产入口一律走 NewConfigured
// （那里会把全部 `ZCODE_*` 环境变量都传进来）。
func ConfiguredWithAdminKey(adminKey string) Configured {
	return NewConfigured(adminKey,
		constants.DefaultGatewayKey,
		constants.DefaultQuotaRefreshInterval,
		constants.DefaultAccountConcurrency,
		constants.DefaultClaimRoundInterval,
	)
}

// NormalizeAdminKey 把「配置给定的初始后台密码」归一：未配置（空串）时回落到
// `constants.FallbackAdminKey`。
//
// 为什么把空串当「未配置」：`ZCODE_ADMIN_KEY=`（显式空）在靶机上会让
// `DEFAULT_ADMIN_KEY` 变成空串，而空串是**非法后台密码**（#9 不允许置空），
// 该状态无法自洽，故按未配置处理。
func NormalizeAdminKey(k string) string {
	if k == "" {
		return constants.FallbackAdminKey
	}
	return k
}

// Defaults 返回全新数据目录下的默认值，全部取自配置初值。
func Defaults(c Configured) Settings {
	c.AdminKey = NormalizeAdminKey(c.AdminKey)
	return Settings{
		AdminKey:             c.AdminKey,
		GatewayKey:           c.GatewayKey,
		QuotaRefreshInterval: c.QuotaRefreshInterval,
		AccountConcurrency:   c.AccountConcurrency,
		ClaimRoundInterval:   c.ClaimRoundInterval,
		Configured:           c,
	}
}

// Load 从存储读出设置；缺失的键回落到默认值。
func Load(ms MetaStore, c Configured) (Settings, error) {
	s := Defaults(c)
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
func Apply(ms MetaStore, p Patch, c Configured) (Settings, error) {
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
	return Load(ms, c)
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
//
// `admin_key_is_default` 比的是**配置给定的默认值**（`s.Configured.AdminKey`），
// 不是字面量、也不是库里那份初值 —— 见 Configured 的实测证据。
func (s Settings) View() View {
	return View{
		AdminKeySet:          s.AdminKey != "",
		AdminKeyMasked:       constants.MaskSecret(s.AdminKey),
		AdminKeyIsDefault:    s.AdminKey == NormalizeAdminKey(s.Configured.AdminKey),
		GatewayKeySet:        s.GatewayKey != "",
		GatewayKeyMasked:     constants.MaskSecret(s.GatewayKey),
		QuotaRefreshInterval: s.QuotaRefreshInterval,
		AccountConcurrency:   s.AccountConcurrency,
		ClaimRoundInterval:   s.ClaimRoundInterval,
	}
}
