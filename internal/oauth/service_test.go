package oauth_test

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/LinYuanNull/zcode2api-go/internal/agent"
	"github.com/LinYuanNull/zcode2api-go/internal/oauth"
)

// fakeUpstream 记录调用并返回预设结果，用来断言「何时打上游、打了什么」。
type fakeUpstream struct {
	initFlow agent.OAuthFlow
	initErr  error

	pollStatus string
	pollErr    error

	initCalls int
	pollCalls int
	lastToken string
	lastFlow  string
}

func (f *fakeUpstream) OAuthInit(_ context.Context, token, provider string) (agent.OAuthFlow, error) {
	f.initCalls++
	f.lastToken = token
	if f.initErr != nil {
		return agent.OAuthFlow{}, f.initErr
	}
	return f.initFlow, nil
}

func (f *fakeUpstream) OAuthPoll(_ context.Context, token, flowID string) (string, error) {
	f.pollCalls++
	f.lastToken = token
	f.lastFlow = flowID
	if f.pollErr != nil {
		return "", f.pollErr
	}
	return f.pollStatus, nil
}

var (
	hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)
	hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ── Start ───────────────────────────────────────────────────

// **flow_id 必须沿用上游的**（A5 实测三值全等）。这里把上游的 id 设成一个不可能被
// 本地随机撞出来的值，确保它不是「本地生成后恰好相等」。
func TestStartAdoptsUpstreamFlowID(t *testing.T) {
	up := &fakeUpstream{initFlow: agent.OAuthFlow{
		FlowID:          "feedfacefeedfacefeedfacefeedface",
		AuthorizeURL:    "https://chat.z.ai/api/oauth/authorize?client_id=c&state=s",
		PollToken:       "whatever",
		ExpiresAt:       1791063908,
		PollIntervalSec: 2,
	}}
	reg := oauth.NewRegistry()
	svc := oauth.NewService(reg, up)

	flowID, url, expiresIn, err := svc.Start(context.Background(), "my-label")
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if flowID != up.initFlow.FlowID {
		t.Errorf("flowID = %q，want 上游的 %q", flowID, up.initFlow.FlowID)
	}
	if url != up.initFlow.AuthorizeURL {
		t.Errorf("authorizeURL = %q", url)
	}
	// 上游回的是 `expires_at` 时间戳，管理侧**固定**给 300（A5 实测）。
	if expiresIn != 300 {
		t.Errorf("expiresIn = %d，want 固定 300", expiresIn)
	}
	if up.initCalls != 1 {
		t.Errorf("initCalls = %d", up.initCalls)
	}
	// 发起时用的 Bearer 必须是 64 位小写 hex 且在**进程内生成**（不落盘）。
	if !hex64.MatchString(up.lastToken) {
		t.Errorf("init 的 Bearer = %q，不是 64 位小写 hex", up.lastToken)
	}

	// 会话已登记，且带上了上游给的 poll_interval_sec（仅供展示，不是门控）。
	f, ok := reg.Lookup(flowID)
	if !ok {
		t.Fatal("Start 之后会话未登记")
	}
	if f.Token != up.lastToken {
		t.Errorf("登记的 Token = %q，want %q（poll 必须复用 init 的 Bearer）", f.Token, up.lastToken)
	}
	if f.Status != oauth.StatusPending {
		t.Errorf("初始 status = %q，want pending", f.Status)
	}
	if f.PollIntervalSec != 2 {
		t.Errorf("PollIntervalSec = %d", f.PollIntervalSec)
	}
	if f.Label != "my-label" {
		t.Errorf("Label = %q", f.Label)
	}
}

// 上游失败时不登记半截会话（否则会留下一个打不开的僵尸 flow）。
func TestStartUpstreamErrorLeavesNoSession(t *testing.T) {
	up := &fakeUpstream{initErr: errors.New("上游不可达")}
	reg := oauth.NewRegistry()
	svc := oauth.NewService(reg, up)

	if _, _, _, err := svc.Start(context.Background(), ""); err == nil {
		t.Fatal("上游失败时 Start 应报错")
	}
	// 用一个不可能存在的 id 反查：全新表里 Lookup 一定失败。
	if _, ok := reg.Lookup("anything"); ok {
		t.Error("上游失败后不应留下会话")
	}
}

// 上游回包缺字段 ⇒ 明确报错（不猜默认值）。
func TestStartRejectsIncompleteUpstreamReply(t *testing.T) {
	for name, flow := range map[string]agent.OAuthFlow{
		"缺 flow_id":       {AuthorizeURL: "https://x/y"},
		"缺 authorize_url": {FlowID: "abcdefabcdefabcdefabcdefabcdefab"},
	} {
		flow := flow
		t.Run(name, func(t *testing.T) {
			svc := oauth.NewService(oauth.NewRegistry(), &fakeUpstream{initFlow: flow})
			if _, _, _, err := svc.Start(context.Background(), ""); err == nil {
				t.Fatalf("%s 时应报错", name)
			}
		})
	}
}

// 没有上游时（默认装配）发起必须**明确报错**，不能伪造 authorize_url。
func TestStartWithoutUpstreamFails(t *testing.T) {
	svc := oauth.NewService(oauth.NewRegistry(), nil)
	if _, url, _, err := svc.Start(context.Background(), ""); err == nil {
		t.Fatalf("无上游时应报错，实际返回 url=%q", url)
	}
}

// ── Poll ────────────────────────────────────────────────────

// **未知 flow_id ⇒ 本地 expired、零出站**（样本 `12-login-poll-unknown` 是 200 不是 404）。
func TestPollUnknownFlowIsExpiredWithoutOutbound(t *testing.T) {
	up := &fakeUpstream{}
	svc := oauth.NewService(oauth.NewRegistry(), up)

	status, msg, acc := svc.Poll(context.Background(), "no-such-flow")
	if status != oauth.StatusExpired {
		t.Errorf("status = %q，want expired", status)
	}
	if msg != "" || acc != nil {
		t.Errorf("expired 不应带 message/account：%q %v", msg, acc)
	}
	if up.pollCalls != 0 {
		t.Errorf("未知 flow 不应打上游，实际 %d 次", up.pollCalls)
	}
}

// **每次 poll 都逐次打上游**（A5 实测：没有本地时间门控）+ 复用同一个 Bearer。
func TestPollHitsUpstreamEveryTime(t *testing.T) {
	up := &fakeUpstream{
		initFlow:   agent.OAuthFlow{FlowID: "abcabcabcabcabcabcabcabcabcabcab", AuthorizeURL: "https://x/y"},
		pollStatus: oauth.StatusPending,
	}
	reg := oauth.NewRegistry()
	svc := oauth.NewService(reg, up)

	flowID, _, _, err := svc.Start(context.Background(), "")
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	initToken := up.lastToken

	for i := 1; i <= 3; i++ {
		status, _, _ := svc.Poll(context.Background(), flowID)
		if status != oauth.StatusPending {
			t.Fatalf("第 %d 次 poll 的 status = %q，want pending", i, status)
		}
		if up.pollCalls != i {
			t.Fatalf("第 %d 次 poll 后出站次数 = %d，want %d（无时间门控 ⇒ 每次都打）",
				i, up.pollCalls, i)
		}
	}
	if up.lastToken != initToken {
		t.Errorf("poll 的 Bearer = %q，want %q（init 与 poll 复用同一个）", up.lastToken, initToken)
	}
	if up.lastFlow != flowID {
		t.Errorf("poll 路径里的 id = %q，want %q", up.lastFlow, flowID)
	}
}

// **出站失败后停掉该 flow 的上游轮询**：回缓存的最后状态，且后续 poll 零出站。
func TestPollStopsUpstreamAfterError(t *testing.T) {
	up := &fakeUpstream{
		initFlow: agent.OAuthFlow{FlowID: "abcabcabcabcabcabcabcabcabcabcab", AuthorizeURL: "https://x/y"},
		pollErr:  errors.New("上游 429"),
	}
	reg := oauth.NewRegistry()
	svc := oauth.NewService(reg, up)

	flowID, _, _, err := svc.Start(context.Background(), "")
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}

	status, _, _ := svc.Poll(context.Background(), flowID)
	if status != oauth.StatusPending {
		t.Errorf("失败后应回缓存的 pending，实际 %q", status)
	}
	if up.pollCalls != 1 {
		t.Fatalf("pollCalls = %d，want 1", up.pollCalls)
	}

	// 第二次：不再打上游，仍回缓存状态。
	status, _, _ = svc.Poll(context.Background(), flowID)
	if status != oauth.StatusPending {
		t.Errorf("停打后仍应回 pending，实际 %q", status)
	}
	if up.pollCalls != 1 {
		t.Errorf("出站失败后不应再打上游，实际累计 %d 次", up.pollCalls)
	}
	if f, _ := reg.Lookup(flowID); !f.UpstreamStopped {
		t.Error("会话应被标记为 UpstreamStopped")
	}
}

// `ready` / `failed` 是**终态**：直接回缓存，不再打上游。
func TestPollTerminalStatusIsCached(t *testing.T) {
	for _, terminal := range []string{oauth.StatusReady, oauth.StatusFailed} {
		terminal := terminal
		t.Run(terminal, func(t *testing.T) {
			up := &fakeUpstream{pollStatus: oauth.StatusPending}
			reg := oauth.NewRegistry()
			reg.Put(&oauth.Flow{
				ID:     "terminalflow0000000000000000000a",
				Status: terminal,
				Token:  "tok",
			})
			svc := oauth.NewService(reg, up)

			status, _, _ := svc.Poll(context.Background(), "terminalflow0000000000000000000a")
			if status != terminal {
				t.Errorf("status = %q，want %q", status, terminal)
			}
			if up.pollCalls != 0 {
				t.Errorf("终态不应打上游，实际 %d 次", up.pollCalls)
			}
		})
	}
}

// 超过有效期 ⇒ `expired`，且不打上游。用注入时钟把「已过期」造出来。
func TestPollExpiredAfterTTL(t *testing.T) {
	up := &fakeUpstream{
		initFlow:   agent.OAuthFlow{FlowID: "abcabcabcabcabcabcabcabcabcabcab", AuthorizeURL: "https://x/y"},
		pollStatus: oauth.StatusPending,
	}
	reg := oauth.NewRegistry()
	now := time.Unix(1_700_000_000, 0)
	reg.SetClock(func() time.Time { return now })
	svc := oauth.NewService(reg, up)

	flowID, _, _, err := svc.Start(context.Background(), "")
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}

	// 有效期 300 秒：走到 301 秒后。
	now = now.Add(301 * time.Second)

	status, _, _ := svc.Poll(context.Background(), flowID)
	if status != oauth.StatusExpired {
		t.Errorf("status = %q，want expired", status)
	}
	if up.pollCalls != 0 {
		t.Errorf("已过期不应打上游，实际 %d 次", up.pollCalls)
	}
	// 过期是一次性的：状态已落到会话上。
	if f, ok := reg.Lookup(flowID); !ok || f.Status != oauth.StatusExpired {
		t.Errorf("会话状态应落为 expired，实际 %+v", f)
	}
}

// 上游返回空 status ⇒ 保持原状态，不把空串写进会话。
func TestPollEmptyUpstreamStatusKeepsPrevious(t *testing.T) {
	up := &fakeUpstream{
		initFlow:   agent.OAuthFlow{FlowID: "abcabcabcabcabcabcabcabcabcabcab", AuthorizeURL: "https://x/y"},
		pollStatus: "",
	}
	reg := oauth.NewRegistry()
	svc := oauth.NewService(reg, up)

	flowID, _, _, _ := svc.Start(context.Background(), "")
	status, _, _ := svc.Poll(context.Background(), flowID)
	if status != oauth.StatusPending {
		t.Errorf("status = %q，want 保持 pending", status)
	}
	if f, _ := reg.Lookup(flowID); f.Status != oauth.StatusPending {
		t.Errorf("会话状态被空串覆盖: %q", f.Status)
	}
}

// ── 标识生成 ────────────────────────────────────────────────

func TestTokenAndFlowIDShape(t *testing.T) {
	tok := oauth.NewToken()
	if !hex64.MatchString(tok) {
		t.Errorf("NewToken = %q，want 64 位小写 hex", tok)
	}
	if oauth.NewToken() == tok {
		t.Error("两次生成的 Token 不应相同")
	}

	id := oauth.NewFlowID()
	if !hex32.MatchString(id) {
		t.Errorf("NewFlowID = %q，want 32 位小写 hex", id)
	}
}
