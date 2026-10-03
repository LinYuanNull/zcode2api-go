package authadmin_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LinYuanNull/zcode2api-go/internal/authadmin"
)

func req(remote, auth string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/admin/api/verify", nil)
	r.RemoteAddr = remote
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	return r
}

// 缺凭证 → 401 `缺少鉴权凭证`（A1 样本 01-verify-unauthorized 逐字可证）。
func TestMissingCredential(t *testing.T) {
	g := authadmin.New(func() string { return "secret" })
	res := g.Authorize(req("10.0.0.1:1111", ""))
	if res.OK || res.Status != http.StatusUnauthorized || res.Detail != "缺少鉴权凭证" {
		t.Fatalf("缺凭证应 401 缺少鉴权凭证：%+v", res)
	}
}

// 密钥实时读取：改完密码立刻按新值鉴权（PUT /settings 后不必重启）。
func TestKeyReadLive(t *testing.T) {
	cur := "old"
	g := authadmin.New(func() string { return cur })
	if res := g.Authorize(req("10.0.0.1:1111", "Bearer old")); !res.OK {
		t.Fatalf("旧值应通过：%+v", res)
	}
	cur = "new"
	if res := g.Authorize(req("10.0.0.1:1111", "Bearer old")); res.OK {
		t.Fatal("改密后旧值不应再通过")
	}
	if res := g.Authorize(req("10.0.0.1:1111", "Bearer new")); !res.OK {
		t.Fatalf("新值应通过：%+v", res)
	}
}

// 未配置密钥时 fail-closed：任何凭证都不通过（空串与空串比较会「相等」，
// 这正是必须显式排除 want == "" 的原因）。
func TestEmptyConfiguredKeyFailsClosed(t *testing.T) {
	g := authadmin.New(func() string { return "" })
	for _, tok := range []string{"Bearer x", "Bearer "} {
		if res := g.Authorize(req("10.0.0.1:1111", tok)); res.OK {
			t.Fatalf("未配置密钥时应一律拒绝：%q", tok)
		}
	}
}

// 缺凭证不记账：扫描器/健康检查不带 Authorization 是常态，记账会把正常用户锁掉。
func TestMissingCredentialDoesNotCountTowardLockout(t *testing.T) {
	g := authadmin.New(func() string { return "secret" })
	g.MaxFailures = 3
	for i := 0; i < 50; i++ {
		if res := g.Authorize(req("10.0.0.1:1111", "")); res.Status != http.StatusUnauthorized {
			t.Fatalf("第 %d 次缺凭证不该被锁定：%+v", i+1, res)
		}
	}
}

// 失败达到阈值后锁定，返回 429；锁定是**按 IP** 的，别的 IP 不受影响。
func TestLockoutAfterMaxFailures(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	g := authadmin.New(func() string { return "secret" })
	g.MaxFailures = 3
	g.FailWindow = time.Minute
	g.LockFor = time.Minute
	g.Now = func() time.Time { return now }

	// 前 MaxFailures 次是 401（锁定在「下一次请求」才生效）。
	for i := 0; i < 3; i++ {
		if res := g.Authorize(req("10.0.0.1:1111", "Bearer wrong")); res.Status != http.StatusUnauthorized {
			t.Fatalf("第 %d 次应为 401：%+v", i+1, res)
		}
	}
	res := g.Authorize(req("10.0.0.1:1111", "Bearer wrong"))
	if res.OK || res.Status != http.StatusTooManyRequests {
		t.Fatalf("达到阈值后应 429：%+v", res)
	}
	// 锁定期内**正确密码也不放行**（否则爆破者只要撞对一次就能继续）。
	if res := g.Authorize(req("10.0.0.1:1111", "Bearer secret")); res.Status != http.StatusTooManyRequests {
		t.Fatalf("锁定期内正确密码也应 429：%+v", res)
	}
	// 另一个 IP 不受影响。
	if res := g.Authorize(req("10.0.0.2:2222", "Bearer secret")); !res.OK {
		t.Fatalf("其它 IP 不应被牵连：%+v", res)
	}
}

// 锁定到期后放行；失败窗口过期后计数归零（否则一天里偶发打错几次会累加到锁定）。
func TestLockExpiryAndWindowReset(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	g := authadmin.New(func() string { return "secret" })
	g.MaxFailures = 3
	g.FailWindow = time.Minute
	g.LockFor = 30 * time.Second
	g.Now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		g.Authorize(req("10.0.0.1:1111", "Bearer wrong"))
	}
	if res := g.Authorize(req("10.0.0.1:1111", "Bearer wrong")); res.Status != http.StatusTooManyRequests {
		t.Fatalf("应先被锁定：%+v", res)
	}

	now = now.Add(31 * time.Second) // 锁定过期，但失败窗口还没过
	if res := g.Authorize(req("10.0.0.1:1111", "Bearer secret")); !res.OK {
		t.Fatalf("锁定到期后正确密码应放行：%+v", res)
	}

	// 换一个新的窗口：计数应从头开始，2 次失败不足以锁定。
	now = now.Add(2 * time.Minute)
	for i := 0; i < 2; i++ {
		if res := g.Authorize(req("10.0.0.3:3333", "Bearer wrong")); res.Status != http.StatusUnauthorized {
			t.Fatalf("新窗口内第 %d 次失败应为 401：%+v", i+1, res)
		}
	}
	if res := g.Authorize(req("10.0.0.3:3333", "Bearer secret")); !res.OK {
		t.Fatalf("窗口内只失败 2 次，不该被锁：%+v", res)
	}
}

// 成功鉴权清掉该 IP 的失败记录。
func TestSuccessResetsCounter(t *testing.T) {
	g := authadmin.New(func() string { return "secret" })
	g.MaxFailures = 2
	g.Authorize(req("10.0.0.1:1111", "Bearer wrong"))
	if res := g.Authorize(req("10.0.0.1:1111", "Bearer secret")); !res.OK {
		t.Fatalf("正确密码应通过：%+v", res)
	}
	// 计数已清零 ⇒ 再一次失败不该触发锁定。
	if res := g.Authorize(req("10.0.0.1:1111", "Bearer wrong")); res.Status != http.StatusUnauthorized {
		t.Fatalf("清零后再失败一次应是 401 而非 429：%+v", res)
	}
}

func TestBearerParsing(t *testing.T) {
	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{"", "", false},
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},   // 方案名大小写不敏感
		{"BEARER abc", "abc", true},   //
		{"Bearer  abc ", "abc", true}, // 两侧空白
		{"Bearer ", "", false},        // 空 token
		{"abc", "", false},            // 缺方案
		{"Basic abc", "", false},      // 别的方案
		{"BearerX abc", "", false},    // 前缀必须是 "Bearer "
	}
	for _, c := range cases {
		got, ok := authadmin.Bearer(req("10.0.0.1:1", c.header))
		if ok != c.ok || got != c.want {
			t.Fatalf("Bearer(%q) = (%q,%v)，want (%q,%v)", c.header, got, ok, c.want, c.ok)
		}
	}
}

// 只信 RemoteAddr：本服务是本地单机网关，前面没有反代，
// 采信 X-Forwarded-For 等于让客户端自带伪造 IP 绕过失败锁定。
func TestClientIPIgnoresForwardedFor(t *testing.T) {
	r := req("127.0.0.1:54321", "")
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	r.Header.Set("X-Real-IP", "5.6.7.8")
	if got := authadmin.ClientIP(r); got != "127.0.0.1" {
		t.Fatalf("应只取 RemoteAddr：%q", got)
	}
	// RemoteAddr 无端口时原样返回（httptest 手工构造时会出现）。
	r2 := req("nonsense", "")
	if got := authadmin.ClientIP(r2); got != "nonsense" {
		t.Fatalf("无法拆分时应原样返回：%q", got)
	}
}
