package models

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
)

// EpochNow 返回当前时间的 epoch 秒（带小数），与靶机的 `time.time()` 同形。
func EpochNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func randomHex8() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("models: 无法读取随机源: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// NewAPIKeyAccount 构造一条 apiKey 账号 —— 等价于靶机 `POST /admin/api/accounts` 的产出。
//
// 规则来源：observations.md #2（id 形态）、#3（slug）、#4（默认名）、#14（固定 apiKey）。
// 空账号的 `quota`/`plan` 为 `{}`、`plans`/`recent_results` 为 `[]`、`installed_at` 为 null。
func NewAPIKeyAccount(provider, name, secret string, existingForProvider int, fp Fingerprint, createdAt float64) (Account, error) {
	if !constants.IsProvider(provider) {
		return Account{}, fmt.Errorf("不支持的 provider: %s", provider)
	}
	if secret == "" {
		return Account{}, fmt.Errorf("凭据不能为空")
	}
	if name == "" {
		name = DefaultName(provider, existingForProvider)
	}
	slug := Slug(name)
	if slug == "" {
		// 名字全是标点/空白时 slug 会为空。靶机在这种输入下的行为**未采样**，
		// 这里退到 provider，保证 id 一定合法（`<slug>-<8hex>`）。
		slug = provider
	}
	apiKey := secret
	return Account{
		ID:            NewAccountID(slug, randomHex8()),
		Name:          name,
		Provider:      provider,
		Mode:          constants.ModeAPIKey,
		APIKey:        &apiKey,
		Enabled:       true,
		Status:        constants.StatusActive,
		Quota:         json.RawMessage("{}"),
		Plan:          json.RawMessage("{}"),
		Plans:         json.RawMessage("[]"),
		Usage:         json.RawMessage("{}"),
		RecentResults: json.RawMessage("[]"),
		CreatedAt:     createdAt,
		Fingerprint:   fp,
		InstallID:     NewUUID4(),
	}, nil
}
