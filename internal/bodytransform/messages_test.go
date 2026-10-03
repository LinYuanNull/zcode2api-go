// Package bodytransform_test 是 A4 body 变换与路由日志字段的**数据驱动测试**。
//
// 数据源就是契约样本本身（`docs/contract/outbound/fixtures/`），所以样本一改、测试立刻跟着变 ——
// 不需要在测试里另抄一份期望值（抄一份就会两边漂移）。
//
//	Messages  ← outbound-requests.json `pairs[]`（/v1/messages 的保序改写）
//	          ← serialization-probes.json `probes[]`（Python dumps 形态的探针）
//	Inspect   ← route-log-probes.json `field_probes[]`（`>>>` 行三项的取值规则）
package bodytransform_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/LinYuanNull/zcode2api-go/internal/bodytransform"
)

const fixturesDir = "../../docs/contract/outbound/fixtures"

func loadFixture(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixturesDir, name))
	if err != nil {
		t.Fatalf("读夹具 %s 失败: %v", name, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("解析夹具 %s 失败: %v", name, err)
	}
}

// ---------------------------------------------------------------- Messages

type messagesFixture struct {
	Pairs []struct {
		Scenario string `json:"scenario"`
		Endpoint string `json:"endpoint"`
		Attempts int    `json:"attempts"`
		Inbound  string `json:"inbound"`
		Outbound string `json:"outbound"`
	} `json:"pairs"`
}

// TestMessagesMatchesCapturedPairs 用真实出站采样的「入站↔出站配对」逐字节校验。
//
// 逐字节是必须的：出站契约包含**键序**与**空白**（Python `json.dumps` 默认分隔符
// `", "` / `": "`），语义比较会把这些抹掉（observations.md #40/#46）。
func TestMessagesMatchesCapturedPairs(t *testing.T) {
	var fx messagesFixture
	loadFixture(t, "outbound-requests.json", &fx)
	if len(fx.Pairs) == 0 {
		t.Fatal("夹具里没有 pairs")
	}
	for _, p := range fx.Pairs {
		p := p
		t.Run(p.Scenario, func(t *testing.T) {
			if p.Endpoint != "/v1/messages" {
				t.Fatalf("本夹具只应收 /v1/messages 的配对，实际 %q", p.Endpoint)
			}
			got, err := bodytransform.Messages([]byte(p.Inbound))
			if err != nil {
				t.Fatalf("Messages 报错: %v", err)
			}
			if string(got) != p.Outbound {
				t.Errorf("出站 body 不一致\n  入站: %s\n  期望: %s\n  实际: %s",
					p.Inbound, p.Outbound, string(got))
			}
		})
	}
}

type serializationProbeFixture struct {
	Probes []struct {
		ID       string `json:"id"`
		Inbound  string `json:"inbound"`
		Outbound string `json:"outbound"`
	} `json:"probes"`
}

// TestMessagesMatchesSerializationProbes 校验「重新序列化」形态：
// 紧凑 / 乱序 / 1e3 / \u0041 的入站体必须被写成 Python `json.dumps` 的形态。
//
// 这几条探针是把「保原样」与「重新 dumps」分开的关键证据（observations.md #40–#46）。
func TestMessagesMatchesSerializationProbes(t *testing.T) {
	var fx serializationProbeFixture
	loadFixture(t, "serialization-probes.json", &fx)
	if len(fx.Probes) == 0 {
		t.Fatal("夹具里没有 probes")
	}
	for _, p := range fx.Probes {
		p := p
		t.Run(p.ID, func(t *testing.T) {
			got, err := bodytransform.Messages([]byte(p.Inbound))
			if err != nil {
				t.Fatalf("Messages 报错: %v", err)
			}
			if string(got) != p.Outbound {
				t.Errorf("出站 body 不一致\n  入站: %s\n  期望: %s\n  实际: %s",
					p.Inbound, p.Outbound, string(got))
			}
		})
	}
}

// TestMessagesIsNotTheChatRebuild 是一条**防回归**断言：`/v1/messages` 的保序改写
// 与 `/v1/chat/completions` 的重建**必须不是同一个变换**。
//
// 由来：第一版 `outbound-requests.json` 把 chat/completions 的重建产物错配给了
// `m-unknown-model`（见该文件的 `_pairing_fix`）。如果哪天有人把 `Messages` 改成
// 「顺便也重建键序」，这条会立刻失败。
func TestMessagesIsNotTheChatRebuild(t *testing.T) {
	var fx struct {
		Pairs []struct {
			Scenario string `json:"scenario"`
			Endpoint string `json:"endpoint"`
			Inbound  string `json:"inbound"`
			Outbound string `json:"outbound"`
		} `json:"pairs"`
	}
	loadFixture(t, "chat-completions-requests.json", &fx)
	if len(fx.Pairs) == 0 {
		t.Fatal("夹具里没有 pairs")
	}
	const cString = "c-string"
	var found bool
	for _, p := range fx.Pairs {
		if p.Scenario != cString {
			continue
		}
		found = true
		if p.Endpoint != "/v1/chat/completions" {
			t.Fatalf("%s 的 endpoint 应为 /v1/chat/completions，实际 %q", cString, p.Endpoint)
		}
		got, err := bodytransform.Messages([]byte(p.Inbound))
		if err != nil {
			t.Fatalf("Messages 报错: %v", err)
		}
		// 保序改写的结果：键序与入站相同（model, max_tokens, messages），只包了 content。
		const wantKeptOrder = `{"model": "GLM-5.3", "max_tokens": 16, "messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]}`
		if string(got) != wantKeptOrder {
			t.Errorf("Messages 应保持入站键序\n  期望: %s\n  实际: %s", wantKeptOrder, string(got))
		}
		if string(got) == p.Outbound {
			t.Errorf("Messages 竟复现了 chat/completions 的重建产物（键序被重排）:\n  %s", p.Outbound)
		}
	}
	if !found {
		t.Fatalf("夹具里找不到场景 %s", cString)
	}
}

// TestMessagesRejectsNonObjectRoot 覆盖「根不是对象」这一支：应返回 ErrNotObject。
func TestMessagesRejectsNonObjectRoot(t *testing.T) {
	for _, raw := range []string{`[1,2]`, `"x"`, `123`, `null`, `true`} {
		if _, err := bodytransform.Messages([]byte(raw)); !errors.Is(err, bodytransform.ErrNotObject) {
			t.Errorf("入站 %s: 期望 ErrNotObject，实际 %v", raw, err)
		}
	}
}

// ---------------------------------------------------------------- Inspect

type routeLogFixture struct {
	FieldProbes []struct {
		ID      string `json:"id"`
		Request string `json:"request"`
		Expect  struct {
			Model       *string `json:"model"`
			ModelLabel  *string `json:"model_label"`
			Stream      *bool   `json:"stream"`
			StreamLabel *string `json:"stream_label"`
			Preview     *string `json:"preview"`
			Error       string  `json:"error"`
		} `json:"expect"`
		Note string `json:"note"`
	} `json:"field_probes"`
}

// TestInspectRouteLogFields 逐条校验 `>>>` 行三项的取值规则。
//
// 这些规则的「反直觉点」正是本测试的价值：preview 取**最后一条 user** 消息（不是首条）、
// 空串**会**覆盖而已空数组**不会**、`stream` 用 **Python 真值**（`"false"` 也是真值）、
// `model` 缺省印 `-`。
func TestInspectRouteLogFields(t *testing.T) {
	var fx routeLogFixture
	loadFixture(t, "route-log-probes.json", &fx)
	if len(fx.FieldProbes) == 0 {
		t.Fatal("夹具里没有 field_probes")
	}
	for _, p := range fx.FieldProbes {
		p := p
		t.Run(p.ID, func(t *testing.T) {
			info, err := bodytransform.Inspect([]byte(p.Request))
			switch p.Expect.Error {
			case "":
				if err != nil {
					t.Fatalf("期望成功，实际报错: %v", err)
				}
			case "not_object":
				if !errors.Is(err, bodytransform.ErrNotObject) {
					t.Fatalf("期望 ErrNotObject，实际 %v", err)
				}
				return
			case "model_not_string":
				if !errors.Is(err, bodytransform.ErrModelNotString) {
					t.Fatalf("期望 ErrModelNotString，实际 %v", err)
				}
				return
			case "parse":
				if err == nil {
					t.Fatal("期望解析错误，实际成功")
				}
				if errors.Is(err, bodytransform.ErrNotObject) || errors.Is(err, bodytransform.ErrModelNotString) {
					t.Fatalf("期望解析错误，实际是语义错误: %v", err)
				}
				return
			default:
				t.Fatalf("夹具里出现未知的 expect.error: %q", p.Expect.Error)
			}

			if p.Expect.Model != nil && info.Model != *p.Expect.Model {
				t.Errorf("Model = %q，期望 %q", info.Model, *p.Expect.Model)
			}
			if p.Expect.ModelLabel != nil && info.ModelLabel() != *p.Expect.ModelLabel {
				t.Errorf("ModelLabel = %q，期望 %q", info.ModelLabel(), *p.Expect.ModelLabel)
			}
			if p.Expect.Stream != nil && info.Stream != *p.Expect.Stream {
				t.Errorf("Stream = %v，期望 %v", info.Stream, *p.Expect.Stream)
			}
			if p.Expect.StreamLabel != nil && info.StreamLabel() != *p.Expect.StreamLabel {
				t.Errorf("StreamLabel = %q，期望 %q", info.StreamLabel(), *p.Expect.StreamLabel)
			}
			if p.Expect.Preview != nil && info.Preview != *p.Expect.Preview {
				t.Errorf("Preview = %q，期望 %q", info.Preview, *p.Expect.Preview)
			}
		})
	}
}

// TestInspectDoesNotMutateBody 确认 Inspect 是只读的（它与 Messages 可能共用同一份入站字节）。
func TestInspectDoesNotMutateBody(t *testing.T) {
	const raw = `{"model": "GLM-5.3", "messages": [{"role": "user", "content": "hi"}]}`
	if _, err := bodytransform.Inspect([]byte(raw)); err != nil {
		t.Fatalf("Inspect 报错: %v", err)
	}
	// 若 Inspect 改写了 content，Messages 的输出就会带上 text 块包装。
	got, err := bodytransform.Messages([]byte(raw))
	if err != nil {
		t.Fatalf("Messages 报错: %v", err)
	}
	const want = `{"model": "GLM-5.3", "messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]}`
	if string(got) != want {
		t.Errorf("Messages 结果不符合预期\n  期望: %s\n  实际: %s", want, string(got))
	}
}
