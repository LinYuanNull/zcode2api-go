package quota_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/identity"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
	"github.com/LinYuanNull/zcode2api-go/internal/quota"
)

// ── 测试替身 ────────────────────────────────────────────────
//
// 为什么不用 httptest 打假上游：三条端点的**线形状**（URL / 头 / 并发）已由
// `internal/agent` 的单测逐字节钉住；本层要验的是**状态机与门控**
//（什么时候出站、出站几次、共用哪个 request id、什么状态落到账号上）。
// 直接替换 `quota.Upstream` 能把「出站次数」变成可断言的事实，比抓包更直接。

// call 是一次出站的可观测事实。
type call struct {
	url       string
	jwt       string
	requestID string
	fp        identity.QuotaFingerprint
	started   time.Time
}

// barrierUpstream 是一个「三条必须同时在飞」的假上游：每条调用先登记，
// 等三条到齐才一起返回。**顺序执行的实现会在这里超时**——这正是要测的并发语义。
type barrierUpstream struct {
	mu      sync.Mutex
	calls   []call
	arrived int32
	gate    chan struct{}

	// status 是每个端点的响应码（按端点后缀查）。
	status map[string]int
	// fail 非空时该端点直接返回传输错误（模拟网络不可达）。
	fail map[string]bool
}

func newBarrierUpstream() *barrierUpstream {
	return &barrierUpstream{
		gate:   make(chan struct{}),
		status: map[string]int{},
		fail:   map[string]bool{},
	}
}

// setInvalidCredential 复现实测的「凭据失效」三码：usage=404、billing=401/401。
func (u *barrierUpstream) setInvalidCredential() {
	u.status = map[string]int{
		"usage":   http.StatusNotFound,
		"current": http.StatusUnauthorized,
		"balance": http.StatusUnauthorized,
	}
}

func (u *barrierUpstream) handle(url string, jwt, requestID string, fp identity.QuotaFingerprint) (*http.Response, error) {
	u.mu.Lock()
	u.calls = append(u.calls, call{url: url, jwt: jwt, requestID: requestID, fp: fp, started: time.Now()})
	u.mu.Unlock()

	if n := atomic.AddInt32(&u.arrived, 1); n == 3 {
		close(u.gate)
	}
	select {
	case <-u.gate:
	case <-time.After(3 * time.Second):
		// 顺序实现会走到这里 —— 返回错误让断言看得见。
		return nil, errors.New("barrier 超时：三条端点没有并发发出")
	}

	suffix := url[strings.LastIndex(url, "/")+1:]
	if u.fail[suffix] {
		return nil, errors.New("模拟传输失败")
	}
	resp := &http.Response{
		StatusCode: u.status[suffix],
		Header:     http.Header{},
		Body:       http.NoBody,
	}
	if _, ok := u.status[suffix]; !ok {
		resp.StatusCode = http.StatusOK
	}
	return resp, nil
}

func (u *barrierUpstream) GetPlanUsage(_ context.Context, jwt, rid string, fp identity.QuotaFingerprint) (*http.Response, error) {
	return u.handle("https://zcode.z.ai/api/v1/zcode-plan/usage", jwt, rid, fp)
}
func (u *barrierUpstream) GetBillingCurrent(_ context.Context, jwt, rid string, fp identity.QuotaFingerprint) (*http.Response, error) {
	return u.handle("https://zcode.z.ai/api/v1/zcode-plan/billing/current", jwt, rid, fp)
}
func (u *barrierUpstream) GetBillingBalance(_ context.Context, jwt, rid string, fp identity.QuotaFingerprint) (*http.Response, error) {
	return u.handle("https://zcode.z.ai/api/v1/zcode-plan/billing/balance", jwt, rid, fp)
}

func (u *barrierUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

func (u *barrierUpstream) snapshot() []call {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]call, len(u.calls))
	copy(out, u.calls)
	return out
}

// fakeAccounts 是内存账号池。
type fakeAccounts struct {
	mu   sync.Mutex
	byID map[string]models.Account
	puts int
}

func newFakeAccounts(accts ...models.Account) *fakeAccounts {
	f := &fakeAccounts{byID: map[string]models.Account{}}
	for _, a := range accts {
		f.byID[a.ID] = a
	}
	return f
}

func (f *fakeAccounts) Get(id string) (models.Account, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.byID[id]
	return a, ok
}

func (f *fakeAccounts) Put(a models.Account) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[a.ID] = a
	f.puts++
	return nil
}

func (f *fakeAccounts) List() []models.Account {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]models.Account, 0, len(f.byID))
	for _, a := range f.byID {
		out = append(out, a)
	}
	return out
}

// ── 夹具 ────────────────────────────────────────────────────

func jwtAccount() models.Account {
	tok := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.sig"
	return models.Account{
		ID:       "a5-jwt-probe-ef234ed1",
		Name:     "a5-jwt-probe",
		Provider: constants.ProviderZAI,
		Mode:     constants.ModeJWT,
		JWTToken: &tok,
		Enabled:  true,
		Status:   constants.StatusActive,
		Fingerprint: models.Fingerprint{
			Platform: "darwin", Arch: "arm64", OSVersion: "24.5.0",
			Language: "ja-JP", Timezone: "Asia/Tokyo", Screen: "1728x1117",
			DeviceMID: "11111111-2222-4333-8444-555555555555",
		},
		CreatedAt: 1791063614.669324,
	}
}

func apiKeyAccount() models.Account {
	key := "sk-plain-api-key"
	return models.Account{
		ID: "acc-api", Provider: constants.ProviderZAI, Mode: constants.ModeAPIKey,
		APIKey: &key, Enabled: true, Status: constants.StatusActive,
	}
}

// newService 建一个可观测的服务（固定时钟 + 固定 request id 生成器）。
func newService(t *testing.T, up *barrierUpstream, accts *fakeAccounts, interval int64) (*quota.Service, *int) {
	t.Helper()
	seq := 0
	s := quota.NewService(up, accts, interval).
		SetClock(func() float64 { return 1791063700.0 }).
		SetRequestID(func() string {
			seq++
			return "req-id-" + strings.Repeat("0", 4) + string(rune('a'+seq-1))
		})
	return s, &seq
}

// ── 用例 ────────────────────────────────────────────────────

// 凭据失效：三条并发、共用同一个 X-Request-Id、全套指纹头；账号落 invalid；
// 结果走 **FRESH** 形态（`result.error`，不是 `message`）。
func TestProbeInvalidCredential(t *testing.T) {
	acc := jwtAccount()
	up := newBarrierUpstream()
	up.setInvalidCredential()
	accts := newFakeAccounts(acc)
	svc, _ := newService(t, up, accts, 0)

	res, err := svc.Refresh(acc)
	if err != nil {
		t.Fatalf("Refresh 失败: %v", err)
	}

	// 出站次数与并发：三条**同时**在飞（barrier 没超时即证明）。
	calls := up.snapshot()
	if len(calls) != 3 {
		t.Fatalf("出站 %d 条，want 3", len(calls))
	}
	if lo, hi := spread(calls); hi.Sub(lo) > 200*time.Millisecond {
		t.Errorf("三条端点不是并发发出的：时间差 %v", hi.Sub(lo))
	}

	// 同一批共用**同一个** X-Request-Id（实测契约）。
	rid := calls[0].requestID
	for _, c := range calls {
		if c.requestID != rid {
			t.Errorf("同批的 X-Request-Id 不一致: %q vs %q", c.requestID, rid)
		}
		if c.jwt != acc.Credential() {
			t.Errorf("Authorization 用的不是账号自己的 JWT: %q", c.jwt)
		}
	}
	// 三条端点各打一次（不是重复打同一条）。
	if got := endpointSet(calls); len(got) != 3 {
		t.Errorf("端点集合 = %v，want usage/current/balance 各一", got)
	}

	// 指纹投影：screen **不**进指纹头，device_mid 要带上。
	want := identity.QuotaFingerprint{
		Platform: "darwin", Arch: "arm64", OSVersion: "24.5.0",
		Language: "ja-JP", Timezone: "Asia/Tokyo",
		DeviceMID: "11111111-2222-4333-8444-555555555555",
	}
	for _, c := range calls {
		if c.fp != want {
			t.Errorf("指纹投影不对:\n got %+v\nwant %+v", c.fp, want)
		}
	}

	// 结果：FRESH 形态。
	if res.OK {
		t.Error("凭据失效时 OK 应为 false")
	}
	if res.Failure != quota.InvalidCredentialMessage {
		t.Errorf("Failure = %q，want %q", res.Failure, quota.InvalidCredentialMessage)
	}
	if res.Message != "" {
		t.Errorf("FRESH 形态不应带 message，实际 %q", res.Message)
	}

	// 账号状态机：invalid + last_error + last_checked_at。
	stored, ok := accts.Get(acc.ID)
	if !ok {
		t.Fatal("账号不见了")
	}
	if stored.Status != constants.StatusInvalid {
		t.Errorf("status = %q，want invalid", stored.Status)
	}
	if stored.LastError == nil || *stored.LastError != quota.InvalidCredentialMessage {
		t.Errorf("last_error = %v，want %q", stored.LastError, quota.InvalidCredentialMessage)
	}
	if stored.LastCheckedAt == nil || *stored.LastCheckedAt != 1791063700.0 {
		t.Errorf("last_checked_at = %v，want 1791063700", stored.LastCheckedAt)
	}
	// 回带的 account 必须是**改写后**的那份。
	if res.Account == nil || res.Account.Status != constants.StatusInvalid {
		t.Errorf("回带的 account 未反映 invalid: %+v", res.Account)
	}
}

// 已 invalid 的账号：**零出站**，走 CACHED 形态（顶层 `message`）。
func TestInvalidAccountNeverQueriesUpstream(t *testing.T) {
	msg := quota.InvalidCredentialMessage
	acc := jwtAccount()
	acc.Status = constants.StatusInvalid
	acc.LastError = &msg

	up := newBarrierUpstream()
	accts := newFakeAccounts(acc)
	svc, _ := newService(t, up, accts, 0)

	res, err := svc.Refresh(acc)
	if err != nil {
		t.Fatalf("Refresh 失败: %v", err)
	}
	if up.count() != 0 {
		t.Errorf("invalid 账号不该出站，实际 %d 条", up.count())
	}
	if res.Failure != "" {
		t.Errorf("CACHED 形态不该带 result，实际 Failure=%q", res.Failure)
	}
	if res.Message != quota.InvalidCredentialMessage {
		t.Errorf("Message = %q，want %q", res.Message, quota.InvalidCredentialMessage)
	}
	if accts.puts != 0 {
		t.Errorf("未出站就不该改写账号，实际 Put %d 次", accts.puts)
	}
}

// 非 active（disabled / cooling / exhausted）同样不出站 —— 「只对 active 查上游」。
func TestNonActiveStatusesSkipUpstream(t *testing.T) {
	for _, st := range []string{constants.StatusDisabled, constants.StatusCooling, constants.StatusExhausted} {
		st := st
		t.Run(st, func(t *testing.T) {
			acc := jwtAccount()
			acc.Status = st
			up := newBarrierUpstream()
			svc, _ := newService(t, up, newFakeAccounts(acc), 0)

			if _, err := svc.Refresh(acc); err != nil {
				t.Fatalf("Refresh 失败: %v", err)
			}
			if up.count() != 0 {
				t.Errorf("%s 账号不该出站，实际 %d 条", st, up.count())
			}
		})
	}
}

// 命中 `quota_refresh_interval` 窗：不出站（置 0 才每次真查）。
func TestRefreshIntervalWindow(t *testing.T) {
	lastChecked := 1791063700.0 - 10 // 10 秒前查过

	acc := jwtAccount()
	acc.LastCheckedAt = &lastChecked

	up := newBarrierUpstream()
	up.setInvalidCredential()
	svc, _ := newService(t, up, newFakeAccounts(acc), 1800)

	if _, err := svc.Refresh(acc); err != nil {
		t.Fatalf("Refresh 失败: %v", err)
	}
	if up.count() != 0 {
		t.Errorf("命中时间窗时不该出站，实际 %d 条", up.count())
	}

	// 窗口外（>1800s）⇒ 真查。
	old := 1791063700.0 - 2000
	acc.LastCheckedAt = &old
	up2 := newBarrierUpstream()
	up2.setInvalidCredential()
	svc2, _ := newService(t, up2, newFakeAccounts(acc), 1800)
	if _, err := svc2.Refresh(acc); err != nil {
		t.Fatalf("Refresh 失败: %v", err)
	}
	if up2.count() != 3 {
		t.Errorf("窗口外应出站 3 条，实际 %d", up2.count())
	}
}

// 三条都 2xx ⇒ 成功体未采样 ⇒ **必须显式报错**，且不得改写账号。
func TestSuccessShapeIsExplicitlyUnsampled(t *testing.T) {
	acc := jwtAccount()
	up := newBarrierUpstream() // 默认三条都 200
	accts := newFakeAccounts(acc)
	svc, _ := newService(t, up, accts, 0)

	_, err := svc.Refresh(acc)
	if !errors.Is(err, quota.ErrSuccessShapeUnsampled) {
		t.Fatalf("err = %v，want ErrSuccessShapeUnsampled（绝不伪造 quota/plan）", err)
	}
	if up.count() != 3 {
		t.Errorf("仍应发出三条（判定靠状态码），实际 %d", up.count())
	}
	if accts.puts != 0 {
		t.Errorf("成功分支不该改写账号，实际 Put %d 次", accts.puts)
	}
	if stored, _ := accts.Get(acc.ID); stored.Status != constants.StatusActive {
		t.Errorf("账号状态被改动了: %q", stored.Status)
	}
}

// 未采样的状态组合（例如 usage 成功、billing 401）⇒ 报错，不改写账号。
func TestUnsampledStatusCombinationIsError(t *testing.T) {
	acc := jwtAccount()
	up := newBarrierUpstream()
	up.status = map[string]int{
		"usage":   http.StatusOK,
		"current": http.StatusUnauthorized,
		"balance": http.StatusUnauthorized,
	}
	accts := newFakeAccounts(acc)
	svc, _ := newService(t, up, accts, 0)

	_, err := svc.Refresh(acc)
	if err == nil {
		t.Fatal("未采样组合应当报错")
	}
	if errors.Is(err, quota.ErrSuccessShapeUnsampled) {
		t.Error("这不该被当成「成功体未采样」")
	}
	if !strings.Contains(err.Error(), "未采样") {
		t.Errorf("错误应说明「未采样」并带上状态码，实际: %v", err)
	}
	if accts.puts != 0 {
		t.Errorf("报错时不该改写账号，实际 Put %d 次", accts.puts)
	}
}

// 传输失败 ⇒ 明确报错（不是「凭据失效」——不能把网络问题当账号问题）。
func TestTransportFailureIsNotInvalidCredential(t *testing.T) {
	acc := jwtAccount()
	up := newBarrierUpstream()
	up.status = map[string]int{
		"usage":   http.StatusNotFound,
		"current": http.StatusUnauthorized,
		"balance": http.StatusUnauthorized,
	}
	up.fail["usage"] = true
	accts := newFakeAccounts(acc)
	svc, _ := newService(t, up, accts, 0)

	_, err := svc.Refresh(acc)
	if err == nil {
		t.Fatal("传输失败应当报错")
	}
	if strings.Contains(err.Error(), quota.InvalidCredentialMessage) {
		t.Error("不该把传输失败判成凭据失效")
	}
	if accts.puts != 0 {
		t.Errorf("传输失败时不该把账号判 invalid，实际 Put %d 次", accts.puts)
	}
}

// 非 JWT：不查上游，回固定文案。
func TestNonJWTReturnsFixedMessage(t *testing.T) {
	acc := apiKeyAccount()
	up := newBarrierUpstream()
	svc, _ := newService(t, up, newFakeAccounts(acc), 0)

	res, err := svc.Refresh(acc)
	if err != nil {
		t.Fatalf("Refresh 失败: %v", err)
	}
	if up.count() != 0 {
		t.Errorf("apiKey 账号不该出站，实际 %d 条", up.count())
	}
	if res.Message != quota.NonJWTMessages {
		t.Errorf("Message = %q，want %q", res.Message, quota.NonJWTMessages)
	}
}

// 启动自刷：只挑 active 的 JWT 账号。
func TestRefreshActivesSelectsOnlyActiveJWT(t *testing.T) {
	active := jwtAccount()
	activeLast := 0.0 // 无 last_checked_at ⇒ 不受窗口限制

	invalid := jwtAccount()
	invalid.ID = "acc-invalid"
	invalid.Status = constants.StatusInvalid

	api := apiKeyAccount()

	up := newBarrierUpstream()
	up.setInvalidCredential()
	accts := newFakeAccounts(active, invalid, api)
	svc, _ := newService(t, up, accts, 0)

	n := svc.RefreshActives()
	if n != 1 {
		t.Errorf("RefreshActives = %d，want 1（只有一条 active JWT）", n)
	}
	if up.count() != 3 {
		t.Errorf("应当只为一个账号发三条，实际 %d 条", up.count())
	}
	if stored, _ := accts.Get("acc-invalid"); stored.Status != constants.StatusInvalid {
		t.Errorf("invalid 账号状态被动了: %q", stored.Status)
	}
	_ = activeLast
}

// 生成器**每批只调一次** ⇒ 同批共用一个 X-Request-Id，逐批不同。
//
// 断言的是「每批恰好生成一次」这条实现约束：如果哪天有人把它挪进三条 goroutine，
// 三条就会各带一个 id —— 那正是实测否定过的形状（observations.md 4.1）。
func TestRequestIDGeneratedOncePerBatch(t *testing.T) {
	acc := jwtAccount()
	gens := 0
	gen := func() string {
		gens++
		return fmt.Sprintf("rid-%d", gens)
	}

	batch := func(up *barrierUpstream, accts *fakeAccounts) []call {
		svc := quota.NewService(up, accts, 0).
			SetClock(func() float64 { return 1791063700.0 }).
			SetRequestID(gen)
		if _, err := svc.Refresh(acc); err != nil {
			t.Fatalf("Refresh 失败: %v", err)
		}
		return up.snapshot()
	}

	up1 := newBarrierUpstream()
	up1.setInvalidCredential()
	calls1 := batch(up1, newFakeAccounts(acc))
	if gens != 1 {
		t.Errorf("第 1 批生成了 %d 个 X-Request-Id，want 1", gens)
	}
	up2 := newBarrierUpstream()
	up2.setInvalidCredential()
	calls2 := batch(up2, newFakeAccounts(acc))
	if gens != 2 {
		t.Errorf("两批共生成 %d 个 X-Request-Id，want 2", gens)
	}

	if calls1[0].requestID == calls2[0].requestID {
		t.Errorf("两批的 X-Request-Id 相同（%q）—— 契约是「同批共用、逐批不同」", calls1[0].requestID)
	}
	for _, c := range calls1 {
		if c.requestID != "rid-1" {
			t.Errorf("第 1 批的 X-Request-Id = %q，want rid-1", c.requestID)
		}
	}
	for _, c := range calls2 {
		if c.requestID != "rid-2" {
			t.Errorf("第 2 批的 X-Request-Id = %q，want rid-2", c.requestID)
		}
	}
}

// ── 小工具 ──────────────────────────────────────────────────

func spread(calls []call) (min, max time.Time) {
	min, max = calls[0].started, calls[0].started
	for _, c := range calls[1:] {
		if c.started.Before(min) {
			min = c.started
		}
		if c.started.After(max) {
			max = c.started
		}
	}
	return min, max
}

func endpointSet(calls []call) map[string]bool {
	out := map[string]bool{}
	for _, c := range calls {
		out[c.url] = true
	}
	return out
}
