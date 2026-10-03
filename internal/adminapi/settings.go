package adminapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/httpx"
	"github.com/LinYuanNull/zcode2api-go/internal/settings"
)

// ── GET /admin/api/settings ─────────────────────────────────
//
// 键顺序由 settings.View 的字段声明序保证，与样本
// `17-settings-get.GET.json` 逐字一致。
func (a *API) handleSettingsGet(w http.ResponseWriter, _ *http.Request) {
	s, err := a.d.Settings.Get()
	if err != nil {
		httpx.WriteDetail(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.View())
}

// ── PUT /admin/api/settings ─────────────────────────────────
//
// 部分写：只更新请求里出现的键（observations.md #11）。
//
// 两条额外规则：
//
//   - `admin_key` 为空串 → 400 `后台密钥不能为空`（observations.md #9，样本
//     `18-settings-put-empty-key.PUT.json` 逐字可证）。
//   - **掩码值原样回提交不得写坏密钥**：值里含 `…`（U+2026）或等于 `••••` 时
//     跳过该键。依据是 ModelMux 面板的既有行为约定（「上游对含 … 的值显式跳过」），
//     见 test/verify_zcode_accounts.py 的「掩码密钥原样提交不会被写坏」一项；
//     A1 样本未覆盖，属**外部约定**而非本仓库样本。
func (a *API) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	raw, ok := a.decodeRawObject(w, r)
	if !ok {
		return
	}

	var patch settings.Patch
	if v, present := raw["admin_key"]; present {
		s, err := asJSONString(v)
		if err != nil {
			httpx.WriteDetail(w, http.StatusBadRequest, "后台密钥必须是字符串")
			return
		}
		if s == "" {
			httpx.WriteDetail(w, http.StatusBadRequest, "后台密钥不能为空")
			return
		}
		if !isMasked(s) {
			patch.AdminKey = &s
		}
	}
	if v, present := raw["gateway_key"]; present {
		s, err := asJSONString(v)
		if err != nil {
			httpx.WriteDetail(w, http.StatusBadRequest, "网关密钥必须是字符串")
			return
		}
		// 空串是合法值：表示网关不校验（.env.example 的注释与样本一致）。
		if !isMasked(s) {
			patch.GatewayKey = &s
		}
	}
	for _, it := range []struct {
		key string
		dst **json.RawMessage
	}{
		{"quota_refresh_interval", &patch.QuotaRefreshInterval},
		{"account_concurrency", &patch.AccountConcurrency},
		{"claim_round_interval", &patch.ClaimRoundInterval},
	} {
		if v, present := raw[it.key]; present {
			val := v
			*it.dst = &val
		}
	}

	if _, err := settings.Apply(a.d.Store, patch, a.d.Configured); err != nil {
		httpx.WriteDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 落库后刷新快照：管理面鉴权与网关读的都是快照，「改完立刻生效」靠这一步。
	if _, err := a.d.Settings.Refresh(); err != nil {
		httpx.WriteDetail(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, okTrue)
}

// asJSONString 把原始 JSON 值当字符串读；非字符串（数字/布尔/null/对象）报错。
func asJSONString(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s, nil
}

// isMasked 判断一个密钥值是否是**回显用的掩码**而不是用户输入的新值。
func isMasked(s string) bool {
	return strings.Contains(s, constants.Ellipsis) || s == constants.KeyMaskDots
}
