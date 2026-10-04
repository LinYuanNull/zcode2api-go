package claim

import (
	"errors"
	"strings"
	"testing"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
)

// stubAccounts 是最小的账号池替身（`store.Store` 只用到 `List`）。
type stubAccounts struct{ list []models.Account }

func (s stubAccounts) List() []models.Account { return s.list }

// invalidJWT 造一个「凭据失效的 JWT 账号」。
//
// id / name 逐字取自 A5 旁录 `outbound-admin/fixtures/admin-responses.json`，
// 这样断言能直接与 `body_text` 逐字节比对（不含任何本测试自造的值）。
func invalidJWT() models.Account {
	tok := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.sig"
	return models.Account{
		ID: "a5-jwt-probe-ef234ed1", Name: "a5-jwt-probe", Provider: constants.ProviderZAI,
		Mode: constants.ModeJWT, JWTToken: &tok, Enabled: true, Status: constants.StatusInvalid,
	}
}

func activeJWT() models.Account {
	a := invalidJWT()
	a.Status = constants.StatusActive
	return a
}

func apiKeyAcc() models.Account {
	key := "sk-plain"
	return models.Account{
		ID: "acc-api", Name: "api", Provider: constants.ProviderZAI, Mode: constants.ModeAPIKey,
		APIKey: &key, Enabled: true, Status: constants.StatusActive,
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := models.MarshalNoHTMLEscape(v)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	return string(b)
}

// ── 候选筛选 ────────────────────────────────────────────────

func TestCandidatesOnlyJWT(t *testing.T) {
	pool := []models.Account{invalidJWT(), apiKeyAcc()}
	got := Candidates(pool)
	if len(got) != 1 || got[0].ID != "a5-jwt-probe-ef234ed1" {
		t.Fatalf("候选 = %+v，want 只有那个 JWT 账号", got)
	}
}

func TestFindJWTRejectsAPIKey(t *testing.T) {
	pool := []models.Account{apiKeyAcc()}
	if _, ok := FindJWT(pool, "acc-api"); ok {
		t.Error("apiKey 账号不该被判为 JWT（样本 10-account-refresh-nonjwt）")
	}
	if _, ok := FindJWT(pool, "nope"); ok {
		t.Error("不存在的 id 不该命中")
	}
}

// ── preview ────────────────────────────────────────────────

// 失效账号的预览行必须与旁录样本**逐字节一致**。
//
// 依据：`admin-responses.json` 的 `claim/preview`
// （`{"preview":[{"account_id":…,"account_name":…,"plans":[],"error":…,
// "activated":false,"activation_error":null}]}`）。
//
// ⚠️ 其中 `activation_error` 是**键在、值为 null** —— 这条断言就是防
// 「`string` + `omitempty` 把整个键省掉」那个退化的。
func TestPreviewInvalidAccountMatchesRecordedSample(t *testing.T) {
	svc := NewService(stubAccounts{list: []models.Account{invalidJWT()}})
	rows, err := svc.Preview()
	if err != nil {
		t.Fatalf("Preview 出错: %v", err)
	}
	want := `[{"account_id":"a5-jwt-probe-ef234ed1","account_name":"a5-jwt-probe",` +
		`"plans":[],"error":"凭证失效，请重新授权","activated":false,"activation_error":null}]`
	if got := mustJSON(t, rows); got != want {
		t.Errorf("预览行与旁录不符:\n got %s\nwant %s", got, want)
	}
}

func TestPreviewEmptyPoolIsEmptyArray(t *testing.T) {
	for name, pool := range map[string][]models.Account{
		"空池":        nil,
		"只有 apiKey": {apiKeyAcc()},
	} {
		t.Run(name, func(t *testing.T) {
			rows, err := NewService(stubAccounts{list: pool}).Preview()
			if err != nil {
				t.Fatalf("Preview 出错: %v", err)
			}
			if rows == nil || len(rows) != 0 {
				t.Fatalf("rows = %#v，want 非 nil 的空切片（编码成 [] 而不是 null）", rows)
			}
			if got := mustJSON(t, rows); got != `[]` {
				t.Errorf("编码 = %s，want []", got)
			}
		})
	}
}

// `active` 账号要打真实上游，成功体未采样 ⇒ 必须显式报错（501 由 adminapi 落）。
func TestPreviewActiveIsUnsampled(t *testing.T) {
	svc := NewService(stubAccounts{list: []models.Account{activeJWT()}})
	rows, err := svc.Preview()
	if !errors.Is(err, ErrUnsampled) {
		t.Fatalf("err = %v，want ErrUnsampled", err)
	}
	if rows != nil {
		t.Errorf("报错时不该回带行（哪怕是部分结果也会让调用方分不清真假）: %+v", rows)
	}
	// 错误文案要能让人行动：说清是哪个账号、卡在什么状态。
	for _, want := range []string{"a5-jwt-probe-ef234ed1", "status=active"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误文案缺 %q: %v", want, err)
		}
	}
}

// ── claim ─────────────────────────────────────────────────

// 失效账号的领取结果必须与旁录样本逐字节一致。
//
// 依据：`admin-responses.json` 的 `claim`
// （`{"outcomes":[{"account_id":…,"account_name":…,"ok":false,"message":…}],
// "summary":{"ok":0,"fail":1}}`；summary 由 adminapi 数出来）。
func TestClaimInvalidAccountMatchesRecordedSample(t *testing.T) {
	svc := NewService(stubAccounts{list: []models.Account{invalidJWT()}})
	out, err := svc.Claim(nil)
	if err != nil {
		t.Fatalf("Claim 出错: %v", err)
	}
	want := `[{"account_id":"a5-jwt-probe-ef234ed1","account_name":"a5-jwt-probe",` +
		`"ok":false,"message":"凭证失效，请重新授权"}]`
	if got := mustJSON(t, out); got != want {
		t.Errorf("回执与旁录不符:\n got %s\nwant %s", got, want)
	}
}

// `account_ids` 是**过滤器**：给了就只领那几个，且顺序按池内顺序。
func TestClaimFiltersByAccountIDs(t *testing.T) {
	a1 := invalidJWT()
	a2 := invalidJWT()
	a2.ID, a2.Name = "acc-second", "second"
	svc := NewService(stubAccounts{list: []models.Account{a1, apiKeyAcc(), a2}})

	out, err := svc.Claim([]string{"acc-second"})
	if err != nil {
		t.Fatalf("Claim 出错: %v", err)
	}
	if len(out) != 1 || out[0].AccountID != "acc-second" {
		t.Fatalf("过滤结果 = %+v，want 只有 acc-second", out)
	}

	// 请求里给了不存在的 id ⇒ 结果为空（不是回退成「全部」）。
	out, err = svc.Claim([]string{"ghost"})
	if err != nil {
		t.Fatalf("Claim 出错: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("给了不存在的 id 却回了 %+v", out)
	}
}

// `account_ids` 为空 = 不过滤（推断，见 selectAccounts 的说明）。
func TestClaimEmptyAccountIDsMeansAllCandidates(t *testing.T) {
	a1 := invalidJWT()
	a2 := invalidJWT()
	a2.ID, a2.Name = "acc-second", "second"
	svc := NewService(stubAccounts{list: []models.Account{a1, apiKeyAcc(), a2}})

	for name, ids := range map[string][]string{"nil": nil, "空切片": {}} {
		t.Run(name, func(t *testing.T) {
			out, err := svc.Claim(ids)
			if err != nil {
				t.Fatalf("Claim 出错: %v", err)
			}
			if len(out) != 2 || out[0].AccountID != a1.ID || out[1].AccountID != a2.ID {
				t.Fatalf("结果 = %+v，want 池内顺序的两个 JWT 候选", out)
			}
		})
	}
}

// 只要有一个候选不是失效态就必须整体报错 —— 不把「有样本的行」与「猜的行」
// 混在同一个数组里返回。
func TestClaimActiveIsUnsampled(t *testing.T) {
	pool := []models.Account{invalidJWT(), activeJWT()}
	out, err := NewService(stubAccounts{list: pool}).Claim(nil)
	if !errors.Is(err, ErrUnsampled) {
		t.Fatalf("err = %v，want ErrUnsampled", err)
	}
	if out != nil {
		t.Errorf("报错时不该回带部分结果: %+v", out)
	}
}

// ── manual ────────────────────────────────────────────────

func TestManualInvalidAccountMatchesClaim(t *testing.T) {
	svc := NewService(stubAccounts{list: []models.Account{invalidJWT()}})
	out, err := svc.Manual("a5-jwt-probe-ef234ed1", "captcha-param")
	if err != nil {
		t.Fatalf("Manual 出错: %v", err)
	}
	// 旁录里 `claim/manual` 与 `claim` **逐字同形**，所以直接与那条比。
	want := `{"account_id":"a5-jwt-probe-ef234ed1","account_name":"a5-jwt-probe",` +
		`"ok":false,"message":"凭证失效，请重新授权"}`
	if got := mustJSON(t, out); got != want {
		t.Errorf("手动回执与旁录不符:\n got %s\nwant %s", got, want)
	}
}

func TestManualMissingOrNonJWT(t *testing.T) {
	svc := NewService(stubAccounts{list: []models.Account{invalidJWT(), apiKeyAcc()}})
	for name, id := range map[string]string{
		"不存在":    "ghost",
		"apiKey": "acc-api",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Manual(id, "p"); !errors.Is(err, ErrNotJWT) {
				t.Fatalf("err = %v，want ErrNotJWT", err)
			}
		})
	}
}

func TestManualActiveIsUnsampled(t *testing.T) {
	svc := NewService(stubAccounts{list: []models.Account{activeJWT()}})
	if _, err := svc.Manual("a5-jwt-probe-ef234ed1", "p"); !errors.Is(err, ErrUnsampled) {
		t.Fatalf("err = %v，want ErrUnsampled", err)
	}
}

// nil 池不该 panic（`Service` 允许被装配成「没有账号池」）。
func TestNilPoolIsSafe(t *testing.T) {
	svc := NewService(nil)
	if rows, err := svc.Preview(); err != nil || len(rows) != 0 {
		t.Errorf("Preview = (%v, %v)，want (空, nil)", rows, err)
	}
	if out, err := svc.Claim(nil); err != nil || len(out) != 0 {
		t.Errorf("Claim = (%v, %v)，want (空, nil)", out, err)
	}
	if _, err := svc.Manual("x", "p"); !errors.Is(err, ErrNotJWT) {
		t.Errorf("Manual err = %v，want ErrNotJWT", err)
	}
}

// 占位实现仍必须明确报错（不能因为接上了真实实现就把它改成「静默成功」）。
func TestUnavailableStillErrors(t *testing.T) {
	var u Unavailable
	if _, err := u.Preview(); !errors.Is(err, ErrUpstreamUnavailable) {
		t.Errorf("Preview err = %v", err)
	}
	if _, err := u.Claim(nil); !errors.Is(err, ErrUpstreamUnavailable) {
		t.Errorf("Claim err = %v", err)
	}
	if _, err := u.Manual("x", "y"); !errors.Is(err, ErrUpstreamUnavailable) {
		t.Errorf("Manual err = %v", err)
	}
}
