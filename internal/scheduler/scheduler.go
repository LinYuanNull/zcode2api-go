// Package scheduler 账号调度：按顺序逐个账号尝试，直到成功或池空。
//
// # 语义来源
//
// 本包的每一个分支都对应 `docs/contract/outbound/behavior.md` §3.1 的一行，
// 那张表逐条由探针钉死（`docs/contract/outbound/observations.md` 五之三/五之四）。
// **改动前先读那张表**，不要凭直觉"优化"。
//
// # 三条容易写错、且已被实测纠正的规则
//
//  1. **重试次数是"连首次"**：429 每账号出站 6 次（1 首次 + 5 重试），
//     5xx 每账号出站 4 次（1 首次 + 3 重试）。写成"重试 6 次"会多打一轮。
//  2. **间隔来源不同**：429 读 `Retry-After`（缺头 60s）；5xx **固定 5s**，
//     即使上游带了 `Retry-After: 1` 也不看（探针 `p-500-ra1` 实测 18.5s ≈ 3×5s）。
//  3. **失败分类决定落库，不决定入站响应**：只有"客户端错"（4xx 非特判）与成功
//     才会把上游响应体送到客户端；401/403 的错误体**不会**到客户端，
//     客户端看到的是全失败后的 503。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/LinYuanNull/zcode2api-go/internal/agent"
	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/marks"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
	"github.com/LinYuanNull/zcode2api-go/internal/store"
)

// 重试与冷却参数（behavior.md §3.1 逐字）。
const (
	// rateLimitRetries 是 429 的重试次数（不含首次）。
	rateLimitRetries = 5
	// serverRetries 是 5xx 的重试次数（不含首次）。
	serverRetries = 3
	// serverRetryDelay 是 5xx 的固定间隔（不读 Retry-After）。
	serverRetryDelay = 5 * time.Second
	// defaultRateLimitDelay 是 429 缺 Retry-After 时的间隔。
	defaultRateLimitDelay = 60 * time.Second
	// cooldownDuration 是 5xx / 传输失败后的冷却时长。
	cooldownDuration = 300 * time.Second
)

// recentKeep 是 recent_results 的容量（探针 F：连打 60 次后仍只保留末尾 20 条）。
const recentKeep = 20

// 客户端错分类：这些 4xx 直接透传、不切账号（探针 p-404/p-408/p-422 实测
// 都只出站 1 次）。
//
// ⚠️ 这个集合是**白名单**：不在集合里的 4xx（如 405、409、418）按 behavior.md
// 第三节末尾登记的规则走"客户端错透传"兜底。见 classify。
func isClientError(code int) bool {
	switch code {
	case 400, 404, 408, 422:
		return true
	}
	return false
}

// Outcome 是一次账号尝试的结论。
type Outcome int

const (
	// OutcomeSuccess：2xx，响应体要送给客户端。
	OutcomeSuccess Outcome = iota
	// OutcomeClientError：4xx 客户端错，上游响应体原样透传给客户端，不再试别的账号。
	OutcomeClientError
	// OutcomeTryNext：本次账号失败，换下一个（401/403/402/429 耗尽/5xx 耗尽/传输失败）。
	OutcomeTryNext
	// OutcomeNoAccount：池里没有可用账号。
	OutcomeNoAccount
)

// Result 是一次调度的结果。
type Result struct {
	Outcome Outcome

	// Resp 是成功或客户端错时的上游响应（**body 未读**，由调用方决定读法）。
	// 判据是 `OutcomeSuccess` 或 `OutcomeClientError`。
	Resp *http.Response

	// ClientCode 是客户端错时的上游状态码。
	ClientCode int
	// ClientBody 是客户端错时的上游响应体（已读，便于透传）。
	ClientBody []byte
	// ClientCT 是客户端错时上游的 Content-Type。
	ClientCT string
}

// Request 是一次调度请求。
type Request struct {
	// Body 是**已变换好**的出站体（pyjson 序列化后的字节）。
	Body []byte
	// ReqID 是路由记号用的 16 位 hex。
	ReqID string
	// Model 是**入站请求体里的 model**（`body.get("model") or "-"`）。
	//
	// 成功 detail 文案用的是它而不是上游回执里的模型 —— 探针 B 实测：
	// 入站 `REQ-MODEL` ⇒ `recent_results[].detail` 写 `HTTP 200 · REQ-MODEL · 0.0s`。
	// 空串表示入站没有 model（记号里会印 `-`），此时 detail 的模型段也用 `-`。
	Model string
}

// Scheduler 调度器。
type Scheduler struct {
	store  *store.Store
	client *agent.Client
	marks  marks.Writer

	// sleep 可注入，便于测试把重试间隔压到 0。
	sleep func(ctx context.Context, d time.Duration) error
}

// New 建调度器。`w` 为 nil 时用 stderr。
func New(st *store.Store, c *agent.Client, w marks.Writer) *Scheduler {
	if w == nil {
		w = marks.NewStdout()
	}
	return &Scheduler{store: st, client: c, marks: w, sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Do 逐个账号尝试。
//
// SSE 场景下 `Result.Resp.Body` 仍**未读**，由调用方流式转发。
//
// 记号约定（behavior.md §3.0/§3.1）：**每次请求都有一条 `>>>`（路由层打），
// 失败到底的请求以一行 `<!>` 收尾**。池空（没有候选账号）与「试完全部候选都失败」
// 都打同一句 `无可用账号 / 额度均已耗尽 / 并发已满` —— 基线里 upstream-401 的
// `<!>` 就是这么来的；此行**不区分**具体失败原因（原因看前面的 `[~]` 行）。
func (s *Scheduler) Do(ctx context.Context, req Request) Result {
	candidates := s.candidates()
	if len(candidates) == 0 {
		marks.Fail(s.marks, req.ReqID, "无可用账号 / 额度均已耗尽 / 并发已满")
		return Result{Outcome: OutcomeNoAccount}
	}
	for _, acct := range candidates {
		res, stop := s.attempt(ctx, req, acct)
		if stop {
			return res
		}
	}
	marks.Fail(s.marks, req.ReqID, "无可用账号 / 额度均已耗尽 / 并发已满")
	return Result{Outcome: OutcomeNoAccount}
}

// attempt 试一个账号。返回的 `stop` 为 true 表示**整条链应当结束**
// （成功 / 客户端错 / 池空）；false 表示换下一个账号。
func (s *Scheduler) attempt(ctx context.Context, req Request, acct models.Account) (Result, bool) {
	name := acct.Name

	started := time.Now()
	resp, err := s.client.PostMessages(ctx, acct.Credential(), req.Body)
	if err != nil {
		// 传输层错误：切账号 + 冷却 300s。
		// last_error 带「连接失败: 」前缀而 recent detail 不带（实测差异，别统一）。
		detail := transportDetail(err)
		marks.Notice(s.marks, req.ReqID, "账号 %s 连接失败，切换下一个", name)
		s.markFailure(acct, constants.StatusCooling, cooldownReason(detail), detail)
		return Result{}, false
	}

	code := resp.StatusCode
	switch {
	case code >= 200 && code < 300:
		s.markSuccess(acct, req, code, time.Since(started))
		return Result{Outcome: OutcomeSuccess, Resp: resp}, true

	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		detail := fmt.Sprintf("鉴权失败 HTTP %d", code)
		marks.Notice(s.marks, req.ReqID, "账号 %s 鉴权失败 %d，切换下一个", name, code)
		s.markFailure(acct, constants.StatusInvalid, detail, detail)
		return Result{}, false

	case code == http.StatusPaymentRequired:
		// last_error 与 recent detail **不同**：前者「额度已用完」，后者「额度用完 HTTP 402」。
		marks.Notice(s.marks, req.ReqID, "账号 %s 额度用完，切换下一个", name)
		s.markFailure(acct, constants.StatusExhausted, "额度已用完", "额度用完 HTTP 402")
		return Result{}, false

	case code == http.StatusTooManyRequests:
		return s.retryRateLimited(ctx, req, acct, resp)

	case code >= 500:
		return s.retryServerError(ctx, req, acct, resp)

	default:
		// 未特别分类的 4xx：按 behavior.md 第三节末尾登记的兜底走「客户端错透传」。
		return s.passthroughClientError(ctx, req, acct, resp, code)
	}
}

// retryRateLimited 走 429 分支：读 Retry-After，重试 5 次（连首次共出站 6 次）。
func (s *Scheduler) retryRateLimited(ctx context.Context, req Request, acct models.Account, first *http.Response) (Result, bool) {
	started := time.Now()
	// ⚠️ **先读 Retry-After 再关连接**：首次等待用的就是这个值。
	// 靶机行为（基线 429 场景，13 条记号行）：h-00 的 `（1/5）` 就是 7s，
	// 不是缺省 60s —— 首次响应已经带了 `Retry-After: 7`。
	delay := rateLimitDelayOf(first)
	agent.DrainAndClose(first)
	for k := 1; k <= rateLimitRetries; k++ {
		if !kSameSleep(ctx, s.sleep, delay) {
			// 请求在重试等待期间被取消 ⇒ 按"连接失败"处理不了（没有传输层错误），
			// 记为冷却并换下一个，与 5xx 耗尽的落库口径保持一致。
			s.markFailure(acct, constants.StatusCooling, cooldownReason("重试期间取消"), "重试期间取消")
			return Result{}, false
		}
		marks.Notice(s.marks, req.ReqID, "账号 %s 被限流 429，%s 后重试（%d/%d）",
			acct.Name, delay, k, rateLimitRetries)
		resp, err := s.client.PostMessages(ctx, acct.Credential(), req.Body)
		if err != nil {
			detail := transportDetail(err)
			marks.Notice(s.marks, req.ReqID, "账号 %s 连接失败，切换下一个", acct.Name)
			s.markFailure(acct, constants.StatusCooling, cooldownReason(detail), detail)
			return Result{}, false
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			s.markSuccess(acct, req, resp.StatusCode, time.Since(started))
			return Result{Outcome: OutcomeSuccess, Resp: resp}, true
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			// 重试期间上游换了分类 ⇒ 交回主链按新状态码处理。
			// 递归深度受控：至少已消耗一次重试，且新分类不会再回到 429。
			return s.attemptAgain(ctx, req, acct, resp)
		}
		// 仍是 429 ⇒ 下一轮的间隔重读 Retry-After（实测 7s 场景里每轮都是 7s）。
		delay = rateLimitDelayOf(resp)
		agent.DrainAndClose(resp)
	}
	detail := fmt.Sprintf("429 重试 %d 次耗尽", rateLimitRetries)
	marks.Notice(s.marks, req.ReqID, "账号 %s 429 重试 %d 次耗尽，切换下一个（账号保持可用）",
		acct.Name, rateLimitRetries)
	// 429 耗尽**不改 status、不写 last_error**，只追加 recent_results。
	s.appendRecent(acct, false, detail)
	return Result{}, false
}

// retryServerError 走 5xx 分支：固定 5s 间隔，重试 3 次（连首次共出站 4 次）。
func (s *Scheduler) retryServerError(ctx context.Context, req Request, acct models.Account, first *http.Response) (Result, bool) {
	started := time.Now()
	agent.DrainAndClose(first)
	code := first.StatusCode
	for k := 1; k <= serverRetries; k++ {
		if !kSameSleep(ctx, s.sleep, serverRetryDelay) {
			s.markFailure(acct, constants.StatusCooling, cooldownReason("重试期间取消"), "重试期间取消")
			return Result{}, false
		}
		marks.Notice(s.marks, req.ReqID, "账号 %s 上游 HTTP %d，%s 后重试（%d/%d）",
			acct.Name, code, serverRetryDelay, k, serverRetries)
		resp, err := s.client.PostMessages(ctx, acct.Credential(), req.Body)
		if err != nil {
			detail := transportDetail(err)
			marks.Notice(s.marks, req.ReqID, "账号 %s 连接失败，切换下一个", acct.Name)
			s.markFailure(acct, constants.StatusCooling, cooldownReason(detail), detail)
			return Result{}, false
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			s.markSuccess(acct, req, resp.StatusCode, time.Since(started))
			return Result{Outcome: OutcomeSuccess, Resp: resp}, true
		}
		if resp.StatusCode < 500 || resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
			resp.StatusCode == http.StatusPaymentRequired {
			// 重试期间换了分类 ⇒ 按新分类走（429 要重读 Retry-After，不能沿用 5s）。
			return s.attemptAgain(ctx, req, acct, resp)
		}
		code = resp.StatusCode
		agent.DrainAndClose(resp)
	}
	// 5xx 耗尽 ⇒ 冷却 300s。last_error 比 recent detail 多「上游 」前缀（实测差异）。
	// 文案里 `冷却 300s` **没有**空格（harness upstream-500 对照，2026-10-04）。
	detail := fmt.Sprintf("HTTP %d 重试 %d 次耗尽，冷却", code, serverRetries)
	marks.Notice(s.marks, req.ReqID, "账号 %s 上游 %d 重试耗尽，冷却 %ds，切换下一个",
		acct.Name, code, int(cooldownDuration/time.Second))
	s.markFailure(acct, constants.StatusCooling, "上游 "+detail, detail)
	return Result{}, false
}

// attemptAgain 处理「重试期间上游换了分类」：把新响应交回分类链，
// 但**不重复计入 429/5xx 的重试预算**（该账号的预算已在重试里耗掉一半，
// 实测未覆盖 —— 记在 behavior.md 第六节，Go 侧按"不重试、直接分类"处理）。
func (s *Scheduler) attemptAgain(ctx context.Context, req Request, acct models.Account, resp *http.Response) (Result, bool) {
	started := time.Now()
	code := resp.StatusCode
	switch {
	case code >= 200 && code < 300:
		s.markSuccess(acct, req, code, time.Since(started))
		return Result{Outcome: OutcomeSuccess, Resp: resp}, true
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		agent.DrainAndClose(resp)
		detail := fmt.Sprintf("鉴权失败 HTTP %d", code)
		marks.Notice(s.marks, req.ReqID, "账号 %s 鉴权失败 %d，切换下一个", acct.Name, code)
		s.markFailure(acct, constants.StatusInvalid, detail, detail)
	case code == http.StatusPaymentRequired:
		agent.DrainAndClose(resp)
		marks.Notice(s.marks, req.ReqID, "账号 %s 额度用完，切换下一个", acct.Name)
		s.markFailure(acct, constants.StatusExhausted, "额度已用完", "额度用完 HTTP 402")
	case code == http.StatusTooManyRequests:
		// 回到 429 分支，但**不重置已用掉的预算**：这里只做一次分类动作。
		agent.DrainAndClose(resp)
		detail := fmt.Sprintf("429 重试 %d 次耗尽", rateLimitRetries)
		marks.Notice(s.marks, req.ReqID, "账号 %s 429 重试 %d 次耗尽，切换下一个（账号保持可用）",
			acct.Name, rateLimitRetries)
		s.appendRecent(acct, false, detail)
	default:
		return s.passthroughClientError(ctx, req, acct, resp, code)
	}
	return Result{}, false
}

// passthroughClientError 走「客户端错」分支：读出上游响应体，转储一行，然后
// **不切账号**、把上游响应原样交给客户端。
func (s *Scheduler) passthroughClientError(_ context.Context, req Request, acct models.Account, resp *http.Response, code int) (Result, bool) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	agent.DrainAndClose(resp)
	ct := resp.Header.Get("Content-Type")
	// 顺序实测（harness upstream-400 对照，2026-10-04）：先 <!> 后 [~]。
	marks.Fail(s.marks, req.ReqID, "上游错误 HTTP %d（账号 %s）", code, acct.Name)
	marks.Notice(s.marks, req.ReqID, "上游 %d 完整响应体: %s", code, string(body))
	// 客户端错**不改账号状态**（实测：400 之后账号仍 active、无 recent 追加）。
	return Result{
		Outcome:    OutcomeClientError,
		ClientCode: code,
		ClientBody: body,
		ClientCT:   ct,
	}, true
}

// kSameSleep 是 `sleep` 的一个薄封装，返回是否睡成功。
func kSameSleep(ctx context.Context, f func(context.Context, time.Duration) error, d time.Duration) bool {
	return f(ctx, d) == nil
}

// rateLimitDelayOf 读 Retry-After；缺失或非法时回落到 60s（探针 p-429-noheader：
// 三次间隔共 180s ⇒ 每次 60s）。
func rateLimitDelayOf(resp *http.Response) time.Duration {
	raw := resp.Header.Get("Retry-After")
	if raw == "" {
		return defaultRateLimitDelay
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	return defaultRateLimitDelay
}

// transportDetail 把传输层错误转成落库文案。
//
// 靶机的原文是 httpx 的异常字符串（如
// `Server disconnected without sending a response.`）。**不逐字复刻 httpx 的
// 措辞**——那是上游库的实现细节，且换库就会变。这里用 Go 的错误文本，
// 形状保持 `连接失败: <原因>`；文案差异登记在 behavior.md 第六节。
func transportDetail(err error) string {
	if err == nil {
		return "连接失败: 未知错误"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "连接失败: 请求超时"
	}
	return "连接失败: " + err.Error()
}

// cooldownReason 组成 5xx / 传输失败的 last_error。
func cooldownReason(detail string) string { return detail }
