package claim

import (
	"fmt"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
)

// Accounts 是领取所需的最小账号池视图。
//
// **只有 `List`**：实测 `claim/preview`、`claim`、`claim/manual` 三条全部零出站、
// 只读已存状态（observations.md 第五节），而且本轮没有任何样本显示领取会**改写**
// 账号（`status` / `last_error` / `recent_results` 都没采到变化）。接口只给读，
// 就等于把「不许在这一层偷偷写库」变成编译期约束。
//
// `*store.Store` 满足本接口。
type Accounts interface {
	List() []models.Account
}

// Service 是 Claimer 的真实实现。
//
// 行为范围（逐条对应样本，不是设计选择）：
//
//   - 候选 = JWT 账号；池里没有候选 → 空数组（样本 `13-` / `14-`）。
//   - 候选 `status=invalid` → 产出旁录样本里那一行 / 那一条（零出站）。
//   - 候选**不是** `invalid` → `ErrUnsampled`：真实领取要打上游 + 换验证码票，
//     而领取端点连在观测表里都没有，成功体更无样本。
//
// 「只要有一个候选不是 invalid 就整体报错」是**刻意的**：把「已采样的行」与
// 「猜出来的行」混在同一个数组里返回，会让调用方无法分辨哪部分可信。宁可整条
// 501 并说明是哪个账号、什么状态卡住了。
type Service struct {
	accts Accounts
}

// NewService 建领取服务。
func NewService(accts Accounts) *Service { return &Service{accts: accts} }

// Preview 实现 Claimer。
//
// 依据：旁录 `admin-responses.json` 的 `claim/preview`
// （`{"preview":[{"account_id":…,"account_name":…,"plans":[],"error":,
// "activated":false,"activation_error":null}]}`）。
func (s *Service) Preview() ([]PreviewRow, error) {
	cands := Candidates(s.accounts())
	rows := make([]PreviewRow, 0, len(cands))
	for _, a := range cands {
		if a.Status != constants.StatusInvalid {
			return nil, unsampledErr("预览", a)
		}
		rows = append(rows, invalidPreviewRow(a))
	}
	return rows, nil
}

// Claim 实现 Claimer。
//
// 依据：旁录 `admin-responses.json` 的 `claim`（与 `claim/manual` 同形）
// （`{"outcomes":[{"account_id":…,"account_name":…,"ok":false,"message":}],
// "summary":{"ok":0,"fail":1}}`）。
func (s *Service) Claim(accountIDs []string) ([]Outcome, error) {
	cands := selectAccounts(s.accounts(), accountIDs)
	out := make([]Outcome, 0, len(cands))
	for _, a := range cands {
		if a.Status != constants.StatusInvalid {
			return nil, unsampledErr("领取", a)
		}
		out = append(out, invalidOutcome(a))
	}
	return out, nil
}

// Manual 实现 Claimer：手动领取（带外部 `captcha_verify_param`）。
//
// 三条路由里手动通路与自动通路的**回执同形**（旁录 `claim/manual` 与 `claim`
// 逐字一致），所以失效分支共用 `invalidOutcome`。
//
// 校验顺序与样本一致：先「账号不存在或非 JWT」（404 那条由 adminapi 按样本先回），
// 再判定状态。
func (s *Service) Manual(accountID, _ string) (Outcome, error) {
	acc, ok := FindJWT(s.accounts(), accountID)
	if !ok {
		return Outcome{}, ErrNotJWT
	}
	if acc.Status != constants.StatusInvalid {
		return Outcome{}, unsampledErr("手动领取", acc)
	}
	return invalidOutcome(acc), nil
}

// accounts 取账号池快照（nil 池按空处理）。
func (s *Service) accounts() []models.Account {
	if s.accts == nil {
		return nil
	}
	return s.accts.List()
}

// selectAccounts 取参与领取的候选：`Candidates(pool) ∩ accountIDs`。
//
// `accountIDs` 为空 = **不过滤**（等价于全部候选）。这是**推断**：
// 样本 `14-claim-empty.POST.json` 的请求体是 `{}` 而池是空的，两种情况都产出
// 空 outcomes，分不出「空 = 全部」还是「空 = 一条都不领」；判据取自上游
// Python 侧把 `account_ids` 当可选过滤器的语义（`set(body.get("account_ids") or [])`，
// 旁证见协同仓库 ModelMux 的 `test/fake_zcode.py` 对同一契约的仿真）。
// 已登记进 PROVENANCE 的推断清单，待真实账号补采时一并校准。
//
// 结果**按池内顺序**（不是请求里 id 的顺序）：候选的遍历顺序不该被一次请求打乱。
func selectAccounts(pool []models.Account, accountIDs []string) []models.Account {
	cands := Candidates(pool)
	if len(accountIDs) == 0 {
		return cands
	}
	want := make(map[string]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		want[id] = struct{}{}
	}
	out := make([]models.Account, 0, len(cands))
	for _, a := range cands {
		if _, ok := want[a.ID]; ok {
			out = append(out, a)
		}
	}
	return out
}

// unsampledErr 造一条带上下文（动作 + 账号 + 状态）的未采样错误。
//
// 带上账号与状态是有用的：用户看到 501 时能立刻知道是**哪个**账号卡在**哪一步**，
// 而不是「领取功能没做」这种无法行动的提示。
func unsampledErr(action string, a models.Account) error {
	return fmt.Errorf("%w：%s时账号 %s（%s）处于 status=%s，需要真实上游领取 + 验证码换票",
		ErrUnsampled, action, a.ID, a.Name, a.Status)
}
