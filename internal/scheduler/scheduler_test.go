package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LinYuanNull/zcode2api-go/internal/agent"
	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/marks"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
	"github.com/LinYuanNull/zcode2api-go/internal/store"
)

// 本文件的判据全部来自 docs/contract/outbound/behavior.md §3.1 的分类表
// 与 docs/contract/outbound/observations.md 五之三/五之四的实测记录。
// 每条断言后面标了依据编号，改动前先读那份表。

// recorder 收集记号行。
type recorder struct{ lines []string }

func (r *recorder) MarkLine(line string) { r.lines = append(r.lines, line) }

func (r *recorder) joined() string { return strings.Join(r.lines, "\n") }

func (r *recorder) count(sub string) int {
	n := 0
	for _, l := range r.lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// newFixture 建一个「N 个账号 + 一个假上游」的测试环境。
//
// `handler` 收到每次出站请求；返回状态码与响应体。
// 重试间隔被压到 0（注入 sleep），所以测试不 sleep。
func newFixture(t *testing.T, n int, handler http.HandlerFunc) (*Scheduler, *store.Store, *recorder, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatalf("打开 store 失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	for i := 0; i < n; i++ {
		tok := fmt.Sprintf("tok-%02d", i)
		a := models.Account{
			ID: fmt.Sprintf("h-%02d", i), Name: fmt.Sprintf("h-%02d", i),
			Provider: "zai", Mode: constants.ModeAPIKey, APIKey: &tok,
			Enabled: true, Status: constants.StatusActive, CreatedAt: float64(i),
		}
		if err := st.Put(a); err != nil {
			t.Fatalf("写入账号失败: %v", err)
		}
	}

	ag := agent.New()
	ag.SetMessagesURLForTest(up.URL)

	rec := &recorder{}
	s := New(st, ag, rec)
	s.sleep = func(context.Context, time.Duration) error { return nil }
	return s, st, rec, up
}

// TestClassifyOutcomes 覆盖分类表里「一次尝试就定分类」的四类。
func TestClassifyOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		code       int
		wantStatus string  // 账号落库后的 status
		wantDetail string  // recent_results 最后一条的 detail
		wantLastEr string  // last_error
		wantNotice string  // 记号里应出现的片段
		wantStop   bool    // 是否终止整条链（不切账号）
		wantOut    Outcome // 期望的 Result.Outcome
	}{
		// ⚠️ 401/403/402 **切账号**（2 个账号各出站 1 次 ⇒ 2 次），
		// 与"客户端错透传"（1 次）是不同的分支。
		{
			name: "401 鉴权失败", code: 401,
			wantStatus: constants.StatusInvalid,
			wantDetail: "鉴权失败 HTTP 401", wantLastEr: "鉴权失败 HTTP 401",
			wantNotice: "账号 h-00 鉴权失败 401，切换下一个", wantOut: OutcomeNoAccount,
		},
		{
			name: "403 鉴权失败", code: 403,
			wantStatus: constants.StatusInvalid,
			wantDetail: "鉴权失败 HTTP 403", wantLastEr: "鉴权失败 HTTP 403",
			wantNotice: "账号 h-00 鉴权失败 403，切换下一个", wantOut: OutcomeNoAccount,
		},
		{
			name: "402 额度用完", code: 402,
			wantStatus: constants.StatusExhausted,
			// ⚠️ last_error 与 detail 不同（实测）："额度已用完" vs "额度用完 HTTP 402"
			wantDetail: "额度用完 HTTP 402", wantLastEr: "额度已用完",
			wantNotice: "账号 h-00 额度用完，切换下一个", wantOut: OutcomeNoAccount,
		},
		{
			name: "404 客户端错透传", code: 404,
			wantStatus: constants.StatusActive, // 客户端错**不改账号状态**
			wantStop:   true, wantOut: OutcomeClientError,
		},
		{
			name: "422 客户端错透传", code: 422,
			wantStatus: constants.StatusActive, wantStop: true, wantOut: OutcomeClientError,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code := c.code
			var n int32
			s, st, rec, _ := newFixture(t, 2, func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&n, 1)
				w.WriteHeader(code)
				fmt.Fprintf(w, `{"error":{"message":"m","type":"%d"}}`, code)
			})
			res := s.Do(context.Background(), Request{Body: []byte(`{}`), ReqID: "aaaa", Model: "GLM-5.3"})

			if res.Outcome != c.wantOut {
				t.Fatalf("Outcome = %v, 想要 %v", res.Outcome, c.wantOut)
			}
			if c.wantStop && res.ClientCode != code {
				t.Errorf("ClientCode = %d, 想要 %d", res.ClientCode, code)
			}
			// 客户端错只出站 1 次（不切账号）；其余分类**切账号** ⇒ 2 个账号各 1 次。
			wantOutbound := int32(2)
			if c.wantStop {
				wantOutbound = 1
			}
			if got := atomic.LoadInt32(&n); got != wantOutbound {
				t.Errorf("出站 %d 次, 想要 %d 次", got, wantOutbound)
			}
			if c.wantStop {
				if rec.count("切换下一个") != 0 {
					t.Errorf("客户端错不该打「切换下一个」:\n%s", rec.joined())
				}
				if rec.count("上游错误 HTTP") != 1 {
					t.Errorf("应打一行 <!> 上游错误:\n%s", rec.joined())
				}
				// 客户端错不打 `[~] …完整响应体` 之外的分类行
				if rec.count("完整响应体") != 1 {
					t.Errorf("应打一行完整响应体转储:\n%s", rec.joined())
				}
			} else {
				if rec.count(c.wantNotice) != 1 {
					t.Errorf("缺少记号 %q:\n%s", c.wantNotice, rec.joined())
				}
			}
			// 落库断言
			a, ok := st.Get("h-00")
			if !ok {
				t.Fatal("账号 h-00 不见了")
			}
			if a.Status != c.wantStatus {
				t.Errorf("status = %q, 想要 %q", a.Status, c.wantStatus)
			}
			if c.wantLastEr != "" {
				if a.LastError == nil || *a.LastError != c.wantLastEr {
					t.Errorf("last_error = %v, 想要 %q", a.LastError, c.wantLastEr)
				}
			}
			if c.wantDetail != "" {
				if d := lastDetail(t, a); d != c.wantDetail {
					t.Errorf("recent detail = %q, 想要 %q", d, c.wantDetail)
				}
			}
			// 全失败 ⇒ 两个账号都被试过（客户端错除外）
			if !c.wantStop {
				b, _ := st.Get("h-01")
				if b.Status != c.wantStatus {
					t.Errorf("第二个账号 status = %q, 想要 %q", b.Status, c.wantStatus)
				}
			}
		})
	}
}

// TestSuccessMarksUseCountAndModel 覆盖成功分支的落库。
//
// 依据：探针 B —— 入站 model 是 `REQ-MODEL` 时，detail 写
// `HTTP 200 · REQ-MODEL · 0.0s`（用入站 model，不是上游回执里的）。
func TestSuccessMarksUseCountAndModel(t *testing.T) {
	s, st, rec, _ := newFixture(t, 1, func(w http.ResponseWriter, r *http.Request) {
		// 上游回执里的 model 故意与入站不同，用于证明 detail 取的是入站值。
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"msg_x","model":"UPSTREAM-MODEL","content":[]}`)
	})
	res := s.Do(context.Background(), Request{Body: []byte(`{}`), ReqID: "aaaa", Model: "REQ-MODEL"})
	if res.Outcome != OutcomeSuccess {
		t.Fatalf("Outcome = %v, 想要 Success", res.Outcome)
	}
	a, _ := st.Get("h-00")
	if a.UseCount != 1 {
		t.Errorf("use_count = %d, 想要 1", a.UseCount)
	}
	if a.LastUsedAt == nil {
		t.Error("last_used_at 未写")
	}
	if a.Status != constants.StatusActive {
		t.Errorf("status = %q, 想要 active（成功不改 status）", a.Status)
	}
	if a.LastError != nil {
		t.Errorf("last_error = %v, 想要 nil", *a.LastError)
	}
	d := lastDetail(t, a)
	if !strings.HasPrefix(d, "HTTP 200 · REQ-MODEL · ") {
		t.Errorf("detail = %q, 想要前缀 %q（模型取入站值）", d, "HTTP 200 · REQ-MODEL · ")
	}
	// 耗时是秒、一位小数、带 s 后缀。`·` 是 U+00B7 中点，不是 ASCII 点，
	// 所以判小数点要用 ASCII '.' 计数（"0.0s" 恰好 1 个）。
	if !strings.HasSuffix(d, "s") || strings.Count(d, ".") != 1 {
		t.Errorf("detail = %q, 想要以 `<秒>.<一位小数>s` 结尾", d)
	}
	// 成功只打 `>>>`，无其它记号
	if rec.count(">>>") != 0 {
		t.Errorf("调度器不该打 >>>（那是路由层打的）:\n%s", rec.joined())
	}
	if len(rec.lines) != 0 {
		t.Errorf("成功不应有任何 [~]/<!> 行:\n%s", rec.joined())
	}
}

// TestSuccessModelDashWhenAbsent 入站无 model 时 detail 用 `-`。
func TestSuccessModelDashWhenAbsent(t *testing.T) {
	s, st, _, _ := newFixture(t, 1, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, `{}`)
	})
	s.Do(context.Background(), Request{Body: []byte(`{}`), ReqID: "aaaa", Model: ""})
	a, _ := st.Get("h-00")
	if d := lastDetail(t, a); !strings.Contains(d, "· - ·") {
		t.Errorf("detail = %q, 想要含 `· - ·`", d)
	}
}

// TestRateLimitRetries 覆盖 429：每账号出站 6 次（1 首次 + 5 重试），账号保持可用。
//
// 依据：behavior_diff.py --scenario upstream-429 ⇒ 2 账号 × 6 = 12 条出站。
func TestRateLimitRetries(t *testing.T) {
	var n int32
	s, st, rec, _ := newFixture(t, 2, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"rate","type":"429"}}`)
	})
	res := s.Do(context.Background(), Request{Body: []byte(`{}`), ReqID: "aaaa", Model: "GLM-5.3"})

	if res.Outcome != OutcomeNoAccount {
		t.Fatalf("Outcome = %v, 想要 NoAccount（全 429 ⇒ 503）", res.Outcome)
	}
	if got := atomic.LoadInt32(&n); got != 12 {
		t.Errorf("出站 %d 次, 想要 12 次（2 账号 × 6）", got)
	}
	// 日志：每个账号 5 行「N s 后重试（k/5）」+ 1 行耗尽
	if got := rec.count("后重试（"); got != 10 {
		t.Errorf("重试行 %d 条, 想要 10 条（2 账号 × 5）:\n%s", got, rec.joined())
	}
	if got := rec.count("重试 5 次耗尽，切换下一个（账号保持可用）"); got != 2 {
		t.Errorf("耗尽行 %d 条, 想要 2 条", got)
	}
	// 间隔取自 Retry-After
	if rec.count("7s 后重试") != 10 {
		t.Errorf("未读到 Retry-After: 7\n%s", rec.joined())
	}
	// 落库：status 仍 active、cooling_until 与 last_error 都是 null
	for _, id := range []string{"h-00", "h-01"} {
		a, _ := st.Get(id)
		if a.Status != constants.StatusActive {
			t.Errorf("%s status = %q, 想要 active（429 保持可用）", id, a.Status)
		}
		if a.CoolingUntil != nil {
			t.Errorf("%s cooling_until = %v, 想要 null", id, *a.CoolingUntil)
		}
		if a.LastError != nil {
			t.Errorf("%s last_error = %q, 想要 null", id, *a.LastError)
		}
		if d := lastDetail(t, a); d != "429 重试 5 次耗尽" {
			t.Errorf("%s detail = %q, 想要 %q", id, d, "429 重试 5 次耗尽")
		}
	}
}

// TestRateLimitDefaultDelay 429 缺 Retry-After ⇒ 60s。
//
// 依据：探针 p-429-noheader（三次间隔共 180s）。
func TestRateLimitDefaultDelay(t *testing.T) {
	s, _, rec, _ := newFixture(t, 1, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{}`)
	})
	s.Do(context.Background(), Request{Body: []byte(`{}`), ReqID: "aaaa"})
	if rec.count("1m0s 后重试") == 0 && rec.count("60s 后重试") == 0 {
		t.Errorf("缺 Retry-After 时应回落 60s:\n%s", rec.joined())
	}
}

// TestServerErrorRetries 覆盖 5xx：每账号出站 4 次（1 首次 + 3 重试），冷却 300s。
//
// 依据：behavior_diff.py --scenario upstream-500 ⇒ 2 账号 × 4 = 8 条出站。
func TestServerErrorRetries(t *testing.T) {
	var n int32
	s, st, rec, _ := newFixture(t, 2, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		// 故意带 Retry-After: 1 —— 实测**不读**它（探针 p-500-ra1：总耗时 ≈ 3×5s）。
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(500)
		fmt.Fprint(w, `{"e":1}`)
	})
	res := s.Do(context.Background(), Request{Body: []byte(`{}`), ReqID: "aaaa", Model: "GLM-5.3"})

	if res.Outcome != OutcomeNoAccount {
		t.Fatalf("Outcome = %v, 想要 NoAccount", res.Outcome)
	}
	if got := atomic.LoadInt32(&n); got != 8 {
		t.Errorf("出站 %d 次, 想要 8 次（2 账号 × 4）", got)
	}
	if got := rec.count("5s 后重试（"); got != 6 {
		t.Errorf("5s 重试行 %d 条, 想要 6 条（2 账号 × 3）:\n%s", got, rec.joined())
	}
	// 文案 `冷却 300s` **没有**空格（harness upstream-500 对照实测）。
	if got := rec.count("冷却 300s，切换下一个"); got != 2 {
		t.Errorf("冷却耗尽行 %d 条, 想要 2 条:\n%s", got, rec.joined())
	}
	if rec.count("1s 后重试") != 0 {
		t.Errorf("5xx 不该读 Retry-After:\n%s", rec.joined())
	}
	for _, id := range []string{"h-00", "h-01"} {
		a, _ := st.Get(id)
		if a.Status != constants.StatusCooling {
			t.Errorf("%s status = %q, 想要 cooling", id, a.Status)
		}
		if a.CoolingUntil == nil {
			t.Fatalf("%s cooling_until 未写", id)
		}
		// 实测：cooling_until ≈ now + 300
		if d := *a.CoolingUntil - nowSeconds(); d < 299 || d > 301 {
			t.Errorf("%s cooling_until 距今 %.1fs, 想要 ~300s", id, d)
		}
		// last_error 比 detail 多「上游 」前缀
		if a.LastError == nil || *a.LastError != "上游 HTTP 500 重试 3 次耗尽，冷却" {
			t.Errorf("%s last_error = %v", id, a.LastError)
		}
		if d := lastDetail(t, a); d != "HTTP 500 重试 3 次耗尽，冷却" {
			t.Errorf("%s detail = %q", id, d)
		}
	}
}

// TestServerErrorUsesLatestCode 重试期间上游换 5xx 码 ⇒ 耗尽文案用最后一个码。
//
// ⚠️ 靶机基线里 5xx 全程返回同一个码（`upstream-500` 场景），所以
// "每轮记号印当轮实际码" 还是 "印固定码" **未被实测覆盖**。
// 本实现选择印当轮实际码（对排障更可行动），并登记为有意选择 ——
// 判据只断言"耗尽文案用最后一个码"，不断言中间轮次。
func TestServerErrorUsesLatestCode(t *testing.T) {
	var n int32
	s, st, _, _ := newFixture(t, 1, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) >= 3 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(500)
	})
	s.Do(context.Background(), Request{Body: []byte(`{}`), ReqID: "aaaa"})
	a, _ := st.Get("h-00")
	if d := lastDetail(t, a); d != "HTTP 503 重试 3 次耗尽，冷却" {
		t.Errorf("detail = %q, 想要以最后一个码 503 为准", d)
	}
	if a.LastError == nil || *a.LastError != "上游 HTTP 503 重试 3 次耗尽，冷却" {
		t.Errorf("last_error = %v, 想要以 503 为准", a.LastError)
	}
}

// TestTransportErrorCools 传输层错误 ⇒ 连接失败 + 冷却 300s。
//
// 依据：探针 C（`s-429` 那个 Server disconnected 场景）——
// last_error = `连接失败: <原因>`，detail **不带**「连接失败: 」前缀。
func TestTransportErrorCools(t *testing.T) {
	s, st, rec, _ := newFixture(t, 2, func(w http.ResponseWriter, r *http.Request) {
		// 直接断连：hijack 后 Close。
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter 不支持 Hijack")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	})
	res := s.Do(context.Background(), Request{Body: []byte(`{}`), ReqID: "aaaa"})
	if res.Outcome != OutcomeNoAccount {
		t.Fatalf("Outcome = %v, 想要 NoAccount", res.Outcome)
	}
	if rec.count("连接失败，切换下一个") != 2 {
		t.Errorf("连接失败行 %d 条, 想要 2 条:\n%s", rec.count("连接失败，切换下一个"), rec.joined())
	}
	a, _ := st.Get("h-00")
	if a.Status != constants.StatusCooling {
		t.Errorf("status = %q, 想要 cooling", a.Status)
	}
	if a.LastError == nil || !strings.HasPrefix(*a.LastError, "连接失败: ") {
		t.Errorf("last_error = %v, 想要以 `连接失败: ` 开头", a.LastError)
	}
	if d := lastDetail(t, a); !strings.HasPrefix(d, "连接失败: ") {
		t.Errorf("detail = %q, 想要以 `连接失败: ` 开头", d)
	}
}

// TestCoolingExpiryRevives 冷却到期的账号自动恢复并被选中。
//
// 依据：探针 `probe_cooling_expiry.py` H/I —— 把 cooling_until 拨到过去后，
// 下一次请求成功选中它、status 变回 active、cooling_until 与 last_error 双清。
func TestCoolingExpiryRevives(t *testing.T) {
	s, st, _, _ := newFixture(t, 1, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, `{}`)
	})
	past := nowSeconds() - 1
	expired := models.Account{
		ID: "h-00", Name: "h-00", Provider: "zai", Mode: constants.ModeAPIKey,
		Enabled: true, Status: constants.StatusCooling, CreatedAt: 0,
		CoolingUntil: &past,
	}
	msg := "上游 HTTP 500 重试 3 次耗尽，冷却"
	expired.LastError = &msg
	if err := st.Put(expired); err != nil {
		t.Fatal(err)
	}

	res := s.Do(context.Background(), Request{Body: []byte(`{}`), ReqID: "aaaa", Model: "GLM-5.3"})
	if res.Outcome != OutcomeSuccess {
		t.Fatalf("Outcome = %v, 想要 Success（到期账号应被选中）", res.Outcome)
	}
	a, _ := st.Get("h-00")
	if a.Status != constants.StatusActive {
		t.Errorf("status = %q, 想要 active", a.Status)
	}
	if a.CoolingUntil != nil {
		t.Errorf("cooling_until = %v, 想要 null", *a.CoolingUntil)
	}
	if a.LastError != nil {
		t.Errorf("last_error = %q, 想要 null", *a.LastError)
	}
}

// TestCoolingNotExpiredSkipped 未到期的账号被跳过（⇒ 503）。
//
// 依据：探针 `probe_cooling_expiry.py` I。
func TestCoolingNotExpiredSkipped(t *testing.T) {
	var n int32
	s, _, rec, _ := newFixture(t, 1, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(200)
		fmt.Fprint(w, `{}`)
	})
	future := nowSeconds() + 3600
	cooling := models.Account{
		ID: "h-00", Name: "h-00", Provider: "zai", Mode: constants.ModeAPIKey,
		Enabled: true, Status: constants.StatusCooling, CreatedAt: 0,
		CoolingUntil: &future,
	}
	if err := s.store.Put(cooling); err != nil {
		t.Fatal(err)
	}
	res := s.Do(context.Background(), Request{Body: []byte(`{}`), ReqID: "aaaa"})
	if res.Outcome != OutcomeNoAccount {
		t.Fatalf("Outcome = %v, 想要 NoAccount", res.Outcome)
	}
	if atomic.LoadInt32(&n) != 0 {
		t.Errorf("未到期账号不该被打，出站 %d 次", n)
	}
	// 池空（含「全部在冷却」）也要打 <!> 收尾行（与靶机一致，见 Do 的注释）
	if rec.count("无可用账号 / 额度均已耗尽 / 并发已满") != 1 {
		t.Errorf("应有 <!> 收尾行:\n%s", rec.joined())
	}
}

// TestNoAccountWhenPoolEmpty 池空 ⇒ NoAccount（对应入站 503）。
func TestNoAccountWhenPoolEmpty(t *testing.T) {
	s, _, _, _ := newFixture(t, 0, func(w http.ResponseWriter, r *http.Request) {})
	res := s.Do(context.Background(), Request{Body: []byte(`{}`), ReqID: "aaaa"})
	if res.Outcome != OutcomeNoAccount {
		t.Fatalf("Outcome = %v, 想要 NoAccount", res.Outcome)
	}
}

// TestRecentResultsCap 容量 20：超出的丢最旧。
//
// 依据：探针 F（连打 60 次成功，库里仍是末尾 20 条）。
func TestRecentResultsCap(t *testing.T) {
	s, st, _, _ := newFixture(t, 1, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, `{}`)
	})
	for i := 0; i < 60; i++ {
		s.Do(context.Background(), Request{Body: []byte(`{}`), ReqID: "aaaa", Model: "GLM-5.3"})
	}
	a, _ := st.Get("h-00")
	items := decodeRecent(a.RecentResults)
	if len(items) != recentKeep {
		t.Fatalf("recent_results %d 条, 想要 %d 条", len(items), recentKeep)
	}
	if a.UseCount != 60 {
		t.Errorf("use_count = %d, 想要 60", a.UseCount)
	}
	// 全部是成功条目（60 次都成功）
	for i, it := range items {
		if !it.Ok {
			t.Errorf("第 %d 条 ok=false, 想要 true", i)
		}
	}
}

// TestRecentResultsKeyOrder 条目键序为 ok, at, detail（探针实测形态）。
func TestRecentResultsKeyOrder(t *testing.T) {
	s, st, _, _ := newFixture(t, 1, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, `{}`)
	})
	s.Do(context.Background(), Request{Body: []byte(`{}`), ReqID: "aaaa", Model: "GLM-5.3"})
	a, _ := st.Get("h-00")
	got := string(a.RecentResults)
	// 外层 Account 落盘会紧凑化，但**键序**不变
	for _, want := range []string{`"ok":true`, `"at":`, `"detail":`} {
		if !strings.Contains(got, want) {
			t.Errorf("recent_results 缺 %s: %s", want, got)
		}
	}
	io_, at, det := strings.Index(got, `"ok"`), strings.Index(got, `"at"`), strings.Index(got, `"detail"`)
	if !(io_ < at && at < det) {
		t.Errorf("键序不是 ok,at,detail: %s", got)
	}
}

// TestSuccessDetailUsesZeroPad 耗时的秒数保留一位小数。
func TestSuccessDetailUsesZeroPad(t *testing.T) {
	s, st, _, _ := newFixture(t, 1, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, `{}`)
	})
	s.Do(context.Background(), Request{Body: []byte(`{}`), ReqID: "aaaa", Model: "M"})
	a, _ := st.Get("h-00")
	d := lastDetail(t, a)
	if !strings.HasSuffix(d, "· 0.0s") {
		t.Errorf("detail = %q, 想要以 `· 0.0s` 结尾（一位小数）", d)
	}
}

// lastDetail 取账号 recent_results 最后一条的 detail。
func lastDetail(t *testing.T, a models.Account) string {
	t.Helper()
	items := decodeRecent(a.RecentResults)
	if len(items) == 0 {
		t.Fatalf("recent_results 为空（a=%+v）", a)
	}
	return items[len(items)-1].Detail
}

// 编译期约束：recorder 必须满足 marks.Writer（记号形态由该包保证）。
var _ marks.Writer = (*recorder)(nil)
