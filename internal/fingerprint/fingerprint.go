// Package fingerprint 生成账号的设备档案（`fingerprint`）。
//
// 取值池来自对靶机 400 次换发的采样，见 docs/contract/store/fingerprint-shape.json
// 与 observations.md #18。
//
// ⚠️ **这不是「逐字节复现」**，也不可能：靶机每次换发都是随机取值。本包的目标是
// **同域**（取值落在同一组枚举内、且保持 platform ↔ os_version 的平台一致性），
// 从而让 Go 产出的档案在上游看来同样合法。
//
// A2 的验收（读取既有账号逐字段一致）与生成器无关 —— 那个走 store 的原样透传。
package fingerprint

import (
	"math/rand/v2"

	"github.com/LinYuanNull/zcode2api-go/internal/models"
)

// 观测到的取值池。顺序无意义（取值是随机的），列在这里是为了可审计。
var (
	platforms = []string{"darwin", "win32"}

	// darwin 的 macOS 版本池（形如 2x.y.z）。
	osVersionDarwin = []string{"22.6.0", "23.6.0", "24.5.0", "24.6.0", "25.5.0"}
	// win32 的 Windows 10/11 构建池。
	osVersionWin32 = []string{"10.0.19045", "10.0.22000", "10.0.22621", "10.0.22631", "10.0.26100", "10.0.26200"}

	languages = []string{"de-DE", "en-GB", "en-SG", "en-US", "ja-JP", "ko-KR", "zh-CN"}
	timezones = []string{
		"America/Los_Angeles", "America/New_York", "Asia/Seoul", "Asia/Shanghai",
		"Asia/Singapore", "Asia/Tokyo", "Europe/Berlin", "Europe/London",
	}
	screens = []string{
		"1366x768", "1440x900", "1512x982", "1728x1117",
		"1920x1080", "2560x1440", "2560x1600", "3840x2160",
	}
)

// Generator 产生设备档案。零值不可用，请用 New。
type Generator struct {
	rnd *rand.Rand
}

// New 建一个生成器；不传种子则用随机种子。
func New(seed ...uint64) *Generator {
	var s0, s1 uint64
	if len(seed) >= 2 {
		s0, s1 = seed[0], seed[1]
	} else if len(seed) == 1 {
		s0, s1 = seed[0], seed[0]^0x9e3779b97f4a7c15
	} else {
		s0, s1 = rand.Uint64(), rand.Uint64()
	}
	return &Generator{rnd: rand.New(rand.NewPCG(s0, s1))}
}

func (g *Generator) pick(pool []string) string { return pool[g.rnd.IntN(len(pool))] }

// Next 生成一份新的设备档案。
//
// platform 与 arch / os_version 的一致性按实测相关性处理：win32 只配 x64，
// darwin 以 arm64 为主（也观测到 x64，保留小概率）。
func (g *Generator) Next() models.Fingerprint {
	platform := g.pick(platforms)
	var arch, osVersion string
	switch platform {
	case "win32":
		arch = "x64"
		osVersion = g.pick(osVersionWin32)
	default: // darwin
		if g.rnd.IntN(100) < 85 {
			arch = "arm64"
		} else {
			arch = "x64"
		}
		osVersion = g.pick(osVersionDarwin)
	}
	return models.Fingerprint{
		Platform:  platform,
		Arch:      arch,
		OSVersion: osVersion,
		Language:  g.pick(languages),
		Timezone:  g.pick(timezones),
		Screen:    g.pick(screens),
		DeviceMID: models.NewUUID4(),
	}
}

// Default 是给「不想持有生成器」的调用方用的便捷函数。
func Default() models.Fingerprint { return New().Next() }
