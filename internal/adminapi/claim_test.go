package adminapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LinYuanNull/zcode2api-go/internal/claim"
	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
	"github.com/LinYuanNull/zcode2api-go/internal/store"
)

// stubClaimer 是注入用的领取器：只回放一个预设结果 / 错误。
type stubClaimer struct {
	preview  []claim.PreviewRow
	outcomes []claim.Outcome
	manual   claim.Outcome
	err      error

	seenIDs []string
}

func (s *stubClaimer) Preview() ([]claim.PreviewRow, error) { return s.preview, s.err }

func (s *stubClaimer) Claim(ids []string) ([]claim.Outcome, error) {
	s.seenIDs = append(s.seenIDs, ids...)
	return s.outcomes, s.err
}

func (s *stubClaimer) Manual(id, _ string) (claim.Outcome, error) {
	s.seenIDs = append(s.seenIDs, id)
	return s.manual, s.err
}

// ── 旁录夹具 ────────────────────────────────────────────────

// recordedResponse 是 A5 旁录夹具（入站响应留证）里的一条记录。
type recordedResponse struct {
	Status   int
	BodyText string
}

// loadRecordedResponses 读 `outbound-admin/fixtures/admin-responses.json`，
// 按 label 建索引。
//
// 为什么直接读这份文件而不是把字符串抄进测试：抄一遍就等于又维护一份副本，
// 样本一更新测试还是绿的 —— 那种「契约测试」是假的。
func loadRecordedResponses(t *testing.T) map[string]recordedResponse {
	t.Helper()
	path := filepath.Join("..", "..", "docs", "contract", "outbound-admin", "fixtures", "admin-responses.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取旁录夹具失败（%s）: %v", path, err)
	}
	var doc struct {
		Records []struct {
			Label    string `json:"label"`
			Status   int    `json:"status"`
			BodyText string `json:"body_text"`
		} `json:"records"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("旁录夹具不是合法 JSON: %v", err)
	}
	out := make(map[string]recordedResponse, len(doc.Records))
	for _, r := range doc.Records {
		out[r.Label] = recordedResponse{Status: r.Status, BodyText: r.BodyText}
	}
	return out
}

// storeWithAccount 建一个只放一个账号的库。
func storeWithAccount(t *testing.T, acc models.Account) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatalf("打开账号库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Put(acc); err != nil {
		t.Fatalf("写入账号失败: %v", err)
	}
	return st
}

// invalidAcc 是旁录样本里那个账号：`jwtAcc()` 的 id/name + 已转 `invalid`。
func invalidAcc() models.Account {
	a := jwtAcc()
	a.Status = constants.StatusInvalid
	msg := constants.MsgInvalidCredential
	a.LastError = &msg
	return a
}

// do 发一次管理面请求，返回状态码与响应体。
func do(t *testing.T, api *API, method, path, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)
	out, _ := io.ReadAll(w.Result().Body)
	return w.Result().StatusCode, strings.TrimSpace(string(out))
}

// ── 核心：逐字节复刻旁录样本 ────────────────────────────────

// 三条 claim 路由对「凭据失效」账号的响应必须与 A5 旁录**逐字节一致**。
//
// 这是 A5-4 的主断言：旁录（`fixtures/admin-responses.json`）就是契约本身，
// 键序、`[]` 与 `null`、文案、以及 `summary` 的计数口径全在这一条里钉死。
func TestClaimInvalidCredentialMatchesRecordedResponses(t *testing.T) {
	rec := loadRecordedResponses(t)
	st := storeWithAccount(t, invalidAcc())
	api := New(Deps{Store: st, Claimer: claim.NewService(st)})

	cases := []struct {
		label  string
		method string
		path   string
		body   string
	}{
		{"claim/preview", http.MethodGet, "/admin/api/claim/preview", ""},
		{"claim", http.MethodPost, "/admin/api/claim", `{}`},
		{"claim/manual", http.MethodPost, "/admin/api/claim/manual",
			`{"account_id":"a5-jwt-probe-ef234ed1","captcha_verify_param":"captcha"}`},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			want, ok := rec[c.label]
			if !ok {
				t.Fatalf("旁录夹具里没有 %q 这条记录", c.label)
			}
			status, body := do(t, api, c.method, c.path, c.body)
			if status != want.Status {
				t.Errorf("status = %d，want %d", status, want.Status)
			}
			if body != want.BodyText {
				t.Errorf("响应体与旁录不符:\n got %s\nwant %s", body, want.BodyText)
			}
		})
	}
}

// `claim/manual` 的两条校验分支逐字节复刻样本（400 / 404）。
func TestClaimManualValidationMatchesSamples(t *testing.T) {
	key := "sk-plain"
	nonJWT := models.Account{
		ID: "acc-api", Name: "api", Provider: constants.ProviderZAI, Mode: constants.ModeAPIKey,
		APIKey: &key, Enabled: true, Status: constants.StatusActive,
	}
	st := storeWithAccount(t, nonJWT)
	api := New(Deps{Store: st, Claimer: claim.NewService(st)})

	cases := []struct {
		name   string
		body   string
		status int
		body2  string
	}{
		{"缺 account_id", `{"captcha_verify_param":"x"}`,
			http.StatusBadRequest, `{"detail":"缺少 account_id"}`},
		{"非 JWT / 不存在", `{"account_id":"acc-api","captcha_verify_param":"x"}`,
			http.StatusNotFound, `{"detail":"JWT 账号不存在"}`},
		{"空白 account_id 也算缺", `{"account_id":"   "}`,
			http.StatusBadRequest, `{"detail":"缺少 account_id"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := do(t, api, http.MethodPost, "/admin/api/claim/manual", c.body)
			if status != c.status || body != c.body2 {
				t.Errorf("got (%d, %s)，want (%d, %s)", status, body, c.status, c.body2)
			}
		})
	}
}

// 池里只有 apiKey 账号时，三条路由都保持**空集**样本形状（不是 501）。
func TestClaimEmptyPoolKeepsSampleShape(t *testing.T) {
	key := "sk-plain"
	nonJWT := models.Account{
		ID: "acc-api", Name: "api", Provider: constants.ProviderZAI, Mode: constants.ModeAPIKey,
		APIKey: &key, Enabled: true, Status: constants.StatusActive,
	}
	st := storeWithAccount(t, nonJWT)
	// 故意注入一个「一调用就报错」的领取器：空池必须**先短路**，不碰它。
	api := New(Deps{Store: st, Claimer: &stubClaimer{err: claim.ErrUnsampled}})

	cases := []struct {
		method, path, body, want string
	}{
		{http.MethodGet, "/admin/api/claim/preview", "", `{"preview":[]}`},
		{http.MethodPost, "/admin/api/claim", `{}`,
			`{"outcomes":[],"summary":{"ok":0,"fail":0}}`},
	}
	for _, c := range cases {
		status, body := do(t, api, c.method, c.path, c.body)
		if status != http.StatusOK || body != c.want {
			t.Errorf("%s %s → (%d, %s)，want (200, %s)", c.method, c.path, status, body, c.want)
		}
	}
}

// 未采样 / 占位实现 / 非 JWT ⇒ 501；内部错 ⇒ 502。三条路由口径一致。
func TestClaimErrorStatusMapping(t *testing.T) {
	st := storeWithAccount(t, invalidAcc())
	routes := []struct{ label, method, path, body string }{
		{"preview", http.MethodGet, "/admin/api/claim/preview", ""},
		{"claim", http.MethodPost, "/admin/api/claim", `{}`},
		{"manual", http.MethodPost, "/admin/api/claim/manual",
			`{"account_id":"a5-jwt-probe-ef234ed1"}`},
	}
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"成功路径未采样", claim.ErrUnsampled, http.StatusNotImplemented},
		{"没有上游", claim.ErrUpstreamUnavailable, http.StatusNotImplemented},
		{"非 JWT", claim.ErrNotJWT, http.StatusNotImplemented},
		{"内部错", io.ErrUnexpectedEOF, http.StatusBadGateway},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			api := New(Deps{Store: st, Claimer: &stubClaimer{err: c.err}})
			for _, r := range routes {
				status, body := do(t, api, r.method, r.path, r.body)
				if status != c.want {
					t.Errorf("%s: status = %d，want %d\n%s", r.label, status, c.want, body)
				}
			}
		})
	}
}

// `status=active` 的账号走真实实现时必须 501 —— 绝不因为「看起来能领」就伪造成功。
func TestClaimActiveAccountIsNotImplemented(t *testing.T) {
	st := storeWithAccount(t, jwtAcc()) // jwtAcc() 的 status 是 active
	api := New(Deps{Store: st, Claimer: claim.NewService(st)})

	for _, r := range []struct{ label, method, path, body string }{
		{"preview", http.MethodGet, "/admin/api/claim/preview", ""},
		{"claim", http.MethodPost, "/admin/api/claim", `{}`},
		{"manual", http.MethodPost, "/admin/api/claim/manual",
			`{"account_id":"a5-jwt-probe-ef234ed1"}`},
	} {
		status, body := do(t, api, r.method, r.path, r.body)
		if status != http.StatusNotImplemented {
			t.Errorf("%s: status = %d，want 501\n%s", r.label, status, body)
		}
		if !strings.Contains(body, "未采样") {
			t.Errorf("%s: 501 的说明里应写明「未采样」，实际: %s", r.label, body)
		}
	}
}

// `account_ids` 必须真的传到领取器（是过滤器，不是被忽略的装饰）。
func TestClaimPassesAccountIDsThrough(t *testing.T) {
	a1 := invalidAcc()
	a2 := invalidAcc()
	a2.ID, a2.Name = "acc-second", "second"
	st := storeWithAccount(t, a1)
	if err := st.Put(a2); err != nil {
		t.Fatalf("写入第二个账号失败: %v", err)
	}

	stub := &stubClaimer{outcomes: []claim.Outcome{
		{AccountID: "acc-second", AccountName: "second", OK: true},
	}}
	api := New(Deps{Store: st, Claimer: stub})

	status, body := do(t, api, http.MethodPost, "/admin/api/claim", `{"account_ids":["acc-second"]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d\n%s", status, body)
	}
	if strings.Join(stub.seenIDs, ",") != "acc-second" {
		t.Errorf("传给领取器的 account_ids = %v，want [acc-second]", stub.seenIDs)
	}
	// summary 由**回执**数出来：stub 回一条 ok=true ⇒ {ok:1,fail:0}。
	want := `{"outcomes":[{"account_id":"acc-second","account_name":"second","ok":true}],` +
		`"summary":{"ok":1,"fail":0}}`
	if body != want {
		t.Errorf("响应不符:\n got %s\nwant %s", body, want)
	}
}

// summary 的计数口径：逐条数，不是另算（旁录是 [ok:false]×1 → {ok:0,fail:1}）。
func TestClaimSummaryCountsOutcomes(t *testing.T) {
	st := storeWithAccount(t, invalidAcc())
	stub := &stubClaimer{outcomes: []claim.Outcome{
		{AccountID: "a", AccountName: "A", OK: true, PlanName: "限时免费套餐", Grants: 100},
		{AccountID: "b", AccountName: "B", Message: constants.MsgInvalidCredential},
	}}
	api := New(Deps{Store: st, Claimer: stub})

	status, body := do(t, api, http.MethodPost, "/admin/api/claim", `{}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d\n%s", status, body)
	}
	if !strings.Contains(body, `"summary":{"ok":1,"fail":1}`) {
		t.Errorf("summary 计数不符: %s", body)
	}
	// 成功那条的键序：account_id, account_name, ok, plan_name, grants（零值键被省掉）。
	if !strings.Contains(body, `"ok":true,"plan_name":"限时免费套餐","grants":100`) {
		t.Errorf("成功回执形状不符: %s", body)
	}
}

// 空体（样本 `14-claim-empty.POST.json` 的请求体就是 `{}`）不该被当成解析失败。
func TestClaimAcceptsEmptyBody(t *testing.T) {
	st := storeWithAccount(t, invalidAcc())
	api := New(Deps{Store: st, Claimer: claim.NewService(st)})

	status, body := do(t, api, http.MethodPost, "/admin/api/claim", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d\n%s", status, body)
	}
	if !strings.Contains(body, `"fail":1`) {
		t.Errorf("空体应等价于 {}（全部候选）: %s", body)
	}
}

// captcha-config 在未接 A6 时返回**诚实的空配置**，不是 501 也不是假参数。
func TestCaptchaConfigIsHonestEmptyConfig(t *testing.T) {
	st := storeWithAccount(t, invalidAcc())
	api := New(Deps{Store: st})

	status, body := do(t, api, http.MethodGet, "/admin/api/claim/captcha-config", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d\n%s", status, body)
	}
	want := `{"enabled":false,"scene_id":"","region":"","prefix":""}`
	if body != want {
		t.Errorf("响应不符:\n got %s\nwant %s", body, want)
	}
}
