package adminapi

import (
	"testing"

	"github.com/LinYuanNull/zcode2api-go/internal/constants"
	"github.com/LinYuanNull/zcode2api-go/internal/models"
)

// TestDetectMode 固化凭据形态判据。
//
// 这张表**逐条来自对运行中靶机的实测**（不是推导）：见 docs/contract/admin/SPEC.md
// 「补充实测 · mode 判定」。两个反直觉点必须一起钉住：
//
//   - 内容不校验：`..`（2 个点、无任何合法 base64）也算 jwt；
//   - provider 参与判定：同一串 JWT 交给 bigmodel 会存成 apiKey。
func TestDetectMode(t *testing.T) {
	cases := []struct {
		provider, token, want string
	}{
		// zai：恰好 2 个点 → jwt
		{"zai", "a.b.c", constants.ModeJWT},
		{"zai", "..", constants.ModeJWT},
		{"zai", "eyJhbGciOiJI.eyJzdWIiOiJlMmUifQ.sigzzz", constants.ModeJWT},
		{"zai", "abc.def.ghi", constants.ModeJWT},
		// zai：点数不对 → apiKey
		{"zai", "sk-plain-no-dots", constants.ModeAPIKey},
		{"zai", "a.b", constants.ModeAPIKey},
		{"zai", "a.b.c.d", constants.ModeAPIKey},
		{"zai", "", constants.ModeAPIKey},
		// bigmodel：不论形态一律 apiKey（含合法 JWT）
		{"bigmodel", "a.b.c", constants.ModeAPIKey},
		{"bigmodel", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c", constants.ModeAPIKey},
		{"bigmodel", "sk-plain", constants.ModeAPIKey},
	}
	for _, c := range cases {
		if got := detectMode(c.provider, c.token); got != c.want {
			t.Errorf("detectMode(%q, %q) = %q, want %q", c.provider, c.token, got, c.want)
		}
	}
}

// TestApplyCredential 确认 mode 与字段**同时**被改对 —— 只改 mode 不改字段，
// 会让 JWT 账号带着 `api_key` 空转（凭据取不到，刷新/领取必然失败）。
func TestApplyCredential(t *testing.T) {
	acc, err := models.NewAPIKeyAccount("zai", "n", "a.b.c", 0, models.Fingerprint{}, 0)
	if err != nil {
		t.Fatalf("构造账号失败: %v", err)
	}
	if acc.Mode != constants.ModeAPIKey || acc.APIKey == nil {
		t.Fatalf("前置状态不符：mode=%q api_key=%v", acc.Mode, acc.APIKey)
	}

	applyCredential(&acc, "a.b.c")
	if acc.Mode != constants.ModeJWT {
		t.Errorf("mode 应为 jwt，实际 %q", acc.Mode)
	}
	if acc.APIKey != nil {
		t.Errorf("jwt 账号不应保留 api_key，实际 %q", *acc.APIKey)
	}
	if acc.JWTToken == nil || *acc.JWTToken != "a.b.c" {
		t.Errorf("jwt_token 未落到凭据上：%v", acc.JWTToken)
	}
	if got := acc.Credential(); got != "a.b.c" {
		t.Errorf("Credential() = %q，want %q", got, "a.b.c")
	}

	// 反向：bigmodel 的同一串凭据必须是 apiKey，且 jwt_token 不参与。
	acc2, err := models.NewAPIKeyAccount("bigmodel", "n", "a.b.c", 0, models.Fingerprint{}, 0)
	if err != nil {
		t.Fatalf("构造账号失败: %v", err)
	}
	applyCredential(&acc2, "a.b.c")
	if acc2.Mode != constants.ModeAPIKey || acc2.JWTToken != nil {
		t.Errorf("bigmodel 应为 apiKey 且无 jwt_token，实际 mode=%q jwt=%v", acc2.Mode, acc2.JWTToken)
	}
}
