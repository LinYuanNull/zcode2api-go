package adminapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
	"github.com/LinYuanNull/zcode2api-go/internal/quota"
	"github.com/LinYuanNull/zcode2api-go/internal/store"
)

// stubRefresher 是注入用的额度刷新器：直接给出要装配的 Result。
type stubRefresher struct {
	res quota.Result
	err error

	seen []models.Account
}

func (s *stubRefresher) Refresh(a models.Account) (quota.Result, error) {
	s.seen = append(s.seen, a)
	return s.res, s.err
}

// newAPIForRefresh 建一个带真实 store 的管理 API（免鉴权，只看响应装配）。
func newAPIForRefresh(t *testing.T, acc models.Account, ref quota.Refresher) *API {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatalf("打开账号库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Put(acc); err != nil {
		t.Fatalf("写入账号失败: %v", err)
	}
	return New(Deps{Store: st, Quota: ref})
}

func jwtAcc() models.Account {
	tok := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.sig"
	return models.Account{
		ID: "a5-jwt-probe-ef234ed1", Name: "a5-jwt-probe", Provider: constants.ProviderZAI,
		Mode: constants.ModeJWT, JWTToken: &tok, Enabled: true, Status: constants.StatusActive,
		CreatedAt: 1791063614.669324,
	}
}

// refresh 发一次单账号刷新，返回状态码与响应体。
func refresh(t *testing.T, api *API, id string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/"+id+"/refresh", nil)
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)
	body, _ := io.ReadAll(w.Result().Body)
	return w.Result().StatusCode, bytes.TrimSpace(body)
}

// FRESH 形态（真查了上游）：键序 ok, result, account，且 `result` 是**嵌套对象**。
//
// 依据：fixture `admin-responses.json` 的 `accounts/refresh#1`。
func TestRefreshFreshShape(t *testing.T) {
	acc := jwtAcc()
	updated := acc
	updated.Status = constants.StatusInvalid
	msg := quota.InvalidCredentialMessage
	updated.LastError = &msg

	ref := &stubRefresher{res: quota.Result{OK: false, Failure: msg, Account: &updated}}
	api := newAPIForRefresh(t, acc, ref)

	status, body := refresh(t, api, acc.ID)
	if status != http.StatusOK {
		t.Fatalf("status = %d，want 200（业务失败走 200）\n%s", status, body)
	}
	if got := topLevelKeys(t, body); strings.Join(got, ",") != "ok,result,account" {
		t.Errorf("顶层键序 = %v，want [ok result account]", got)
	}
	// `result` 是嵌套对象 `{"error":…}`，不是字符串 —— 这是与 CACHED 的**唯一**判据。
	if !bytes.Contains(body, []byte(`"result":{"error":"`+quota.InvalidCredentialMessage+`"}`)) {
		t.Errorf("result 不是嵌套的 {error:…}: %s", body)
	}
	if bytes.Contains(body, []byte(`"message"`)) {
		t.Errorf("FRESH 形态不应出现 message: %s", body)
	}
	// account 必须是**改写后**的那份（status=invalid）。
	if !bytes.Contains(body, []byte(`"status":"invalid"`)) {
		t.Errorf("回带的 account 未反映刷新结果: %s", body)
	}
}

// CACHED 形态（未出站）：键序 ok, message, account。
//
// 依据：fixture `admin-responses.json` 的 `accounts/refresh#2`。
func TestRefreshCachedShape(t *testing.T) {
	acc := jwtAcc()
	msg := quota.InvalidCredentialMessage
	acc.LastError = &msg

	ref := &stubRefresher{res: quota.Result{OK: false, Message: msg, Account: &acc}}
	api := newAPIForRefresh(t, acc, ref)

	status, body := refresh(t, api, acc.ID)
	if status != http.StatusOK {
		t.Fatalf("status = %d，want 200\n%s", status, body)
	}
	if got := topLevelKeys(t, body); strings.Join(got, ",") != "ok,message,account" {
		t.Errorf("顶层键序 = %v，want [ok message account]", got)
	}
	if bytes.Contains(body, []byte(`"result"`)) {
		t.Errorf("CACHED 形态不应出现 result: %s", body)
	}
}

// `account` 的键序必须与样本逐字一致（22 键，与 `models.PublicView` 同序）。
func TestRefreshAccountKeyOrder(t *testing.T) {
	acc := jwtAcc()
	ref := &stubRefresher{res: quota.Result{OK: false, Message: "m", Account: &acc}}
	api := newAPIForRefresh(t, acc, ref)

	_, body := refresh(t, api, acc.ID)
	var doc struct {
		Account json.RawMessage `json:"account"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	want := []string{
		"id", "name", "provider", "mode", "token_masked", "enabled", "status",
		"quota", "plan", "plans", "use_count", "fail_count", "risk_strikes",
		"recent_results", "last_used_at", "last_checked_at", "cooling_until",
		"last_error", "created_at", "fingerprint", "install_id", "installed_at",
	}
	if got := topLevelKeys(t, doc.Account); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("account 键序不符：\n got %v\nwant %v", got, want)
	}
}

// 非 JWT：保持样本形状 `{ok:false,message:"仅 Coding Plan (JWT) 账号支持额度查询"}`，
// 且**不**回带 account（样本里没有这个键）。
func TestRefreshNonJWTKeepsSampleShape(t *testing.T) {
	key := "sk-plain"
	acc := models.Account{
		ID: "acc-api", Name: "api", Provider: constants.ProviderZAI, Mode: constants.ModeAPIKey,
		APIKey: &key, Enabled: true, Status: constants.StatusActive,
	}
	ref := &stubRefresher{}
	api := newAPIForRefresh(t, acc, ref)

	status, body := refresh(t, api, acc.ID)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if got := topLevelKeys(t, body); strings.Join(got, ",") != "ok,message" {
		t.Errorf("顶层键序 = %v，want [ok message]", got)
	}
	if !bytes.Contains(body, []byte(quota.NonJWTMessages)) {
		t.Errorf("文案不符: %s", body)
	}
	if len(ref.seen) != 0 {
		t.Errorf("非 JWT 不该调用刷新器，实际 %d 次", len(ref.seen))
	}
}

// 未采样 / 无上游 ⇒ 501；上游侧拿不到可用答案 ⇒ 502。两种都不伪装成功。
func TestRefreshErrorStatusMapping(t *testing.T) {
	acc := jwtAcc()
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"成功体未采样", quota.ErrSuccessShapeUnsampled, http.StatusNotImplemented},
		{"无上游", quota.ErrUpstreamUnavailable, http.StatusNotImplemented},
		{"上游失败", io.ErrUnexpectedEOF, http.StatusBadGateway},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			api := newAPIForRefresh(t, acc, &stubRefresher{err: c.err})
			status, body := refresh(t, api, acc.ID)
			if status != c.want {
				t.Errorf("status = %d，want %d\n%s", status, c.want, body)
			}
		})
	}
}

// 全量刷新：池里没有 JWT 账号时逐字节保持样本的全零形状。
func TestRefreshAllEmptyIsAllZero(t *testing.T) {
	key := "sk-plain"
	acc := models.Account{
		ID: "acc-api", Name: "api", Provider: constants.ProviderZAI, Mode: constants.ModeAPIKey,
		APIKey: &key, Enabled: true, Status: constants.StatusActive,
	}
	api := newAPIForRefresh(t, acc, &stubRefresher{})

	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/refresh",
		strings.NewReader(`{"all":true}`))
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)
	body, _ := io.ReadAll(w.Result().Body)

	want := `{"summary":{"ok":0,"fail":0},"count":0,"skipped_cooling":0,"skipped_invalid":0}`
	if strings.TrimSpace(string(body)) != want {
		t.Errorf("响应不符:\n got %s\nwant %s", body, want)
	}
}

// 全量刷新：有 JWT 候选时按刷新结果汇总，且 count = 实际尝试数。
//
// `count` 的口径是**推断**（样本只有空池那一份，全是 0）：取「实际尝试刷新的账号数」。
// 已登记在 PROVENANCE 的推断清单里。
func TestRefreshAllAggregates(t *testing.T) {
	active := jwtAcc()
	invalid := jwtAcc()
	invalid.ID = "acc-invalid"
	invalid.Status = constants.StatusInvalid
	cooling := jwtAcc()
	cooling.ID = "acc-cooling"
	cooling.Status = constants.StatusCooling

	st, err := store.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatalf("打开账号库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, a := range []models.Account{active, invalid, cooling} {
		if err := st.Put(a); err != nil {
			t.Fatalf("写入账号失败: %v", err)
		}
	}

	msg := quota.InvalidCredentialMessage
	out := active
	out.Status = constants.StatusInvalid
	out.LastError = &msg
	ref := &stubRefresher{res: quota.Result{OK: false, Failure: msg, Account: &out}}
	api := New(Deps{Store: st, Quota: ref})

	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/refresh",
		strings.NewReader(`{"all":true}`))
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)
	body, _ := io.ReadAll(w.Result().Body)

	want := `{"summary":{"ok":0,"fail":1},"count":1,"skipped_cooling":1,"skipped_invalid":1}`
	if strings.TrimSpace(string(body)) != want {
		t.Errorf("响应不符:\n got %s\nwant %s", body, want)
	}
	if len(ref.seen) != 1 || ref.seen[0].ID != active.ID {
		t.Errorf("只有一条候选账号该被刷新，实际 %+v", ref.seen)
	}
}

// topLevelKeys 按序取出 JSON 对象的一层键名（键序是可观测契约的一部分）。
func topLevelKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		t.Fatalf("不是 JSON 对象: %s", raw)
	}
	var keys []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			t.Fatalf("读键失败: %v", err)
		}
		key, _ := kt.(string)
		keys = append(keys, key)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("跳过值失败: %v", err)
		}
	}
	return keys
}
