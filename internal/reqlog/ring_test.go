package reqlog_test

import (
	"encoding/json"
	"testing"

	"github.com/LinYuanNull/zcode2api-go/internal/reqlog"
)

// 空缓冲必须编码成 `[]` 而不是 `null` —— 面板对 `null` 直接遍历会抛。
func TestEmptyEncodesAsArray(t *testing.T) {
	r := reqlog.New(4)
	b, err := json.Marshal(r.Entries())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[]" {
		t.Fatalf("空 entries 应编码为 []，得到 %s", b)
	}
	if r.Len() != 0 || r.Keep() != 4 {
		t.Fatalf("空缓冲：Len=%d Keep=%d", r.Len(), r.Keep())
	}
}

// Entries 必须**从旧到新**（面板按时间正序展示）。
func TestEntriesOldestFirst(t *testing.T) {
	r := reqlog.New(5)
	for _, id := range []string{"a", "b", "c"} {
		r.Add(reqlog.Entry{ID: id})
	}
	got := r.Entries()
	if len(got) != 3 {
		t.Fatalf("条数不符：%d", len(got))
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i].ID != want {
			t.Fatalf("顺序不符（应旧→新）：%v", []string{got[0].ID, got[1].ID, got[2].ID})
		}
	}
}

// 绕圈后仍是「最近 keep 条、从旧到新」：满了覆盖最旧，不是丢弃最新。
func TestRingWrapKeepsNewestInOrder(t *testing.T) {
	r := reqlog.New(3)
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		r.Add(reqlog.Entry{ID: id})
	}
	got := r.Entries()
	if len(got) != 3 || r.Len() != 3 {
		t.Fatalf("容量应为 3：len=%d Len=%d", len(got), r.Len())
	}
	for i, want := range []string{"3", "4", "5"} {
		if got[i].ID != want {
			t.Fatalf("绕圈后应为 3,4,5，得到 %s,%s,%s", got[0].ID, got[1].ID, got[2].ID)
		}
	}
}

// 绕圈满之后 Entries 的切片容量可能超过 keep（两次 append 的产物），
// 但长度必须正好是 keep —— 长度错了面板会多渲染出空行。
func TestWrapLenExactlyKeep(t *testing.T) {
	r := reqlog.New(2)
	for _, id := range []string{"x", "y", "z"} {
		r.Add(reqlog.Entry{ID: id})
	}
	if got := len(r.Entries()); got != 2 {
		t.Fatalf("绕圈后长度应为 2，得到 %d", got)
	}
}

// Clear 后必须回到「空且可继续写」的状态（不是只把长度清零）。
func TestClearResetsAndReusable(t *testing.T) {
	r := reqlog.New(3)
	r.Add(reqlog.Entry{ID: "a"})
	r.Add(reqlog.Entry{ID: "b"})
	r.Clear()
	if r.Len() != 0 || len(r.Entries()) != 0 {
		t.Fatalf("Clear 后应为空：Len=%d", r.Len())
	}
	r.Add(reqlog.Entry{ID: "c"})
	got := r.Entries()
	if len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("Clear 后应能继续写：%+v", got)
	}
}

// keep <= 0 回落到 1：容量为 0 会让 Add 除零 panic。
func TestNonPositiveKeepFallsBackToOne(t *testing.T) {
	for _, k := range []int{0, -3} {
		r := reqlog.New(k)
		if r.Keep() != 1 {
			t.Fatalf("keep=%d 应回落为 1，得到 %d", k, r.Keep())
		}
		r.Add(reqlog.Entry{ID: "a"})
		r.Add(reqlog.Entry{ID: "b"})
		if got := r.Entries(); len(got) != 1 || got[0].ID != "b" {
			t.Fatalf("容量 1 时应只留最新一条：%+v", got)
		}
	}
}

// Entries 返回的是副本：调用方改动不应污染缓冲。
func TestEntriesIsCopy(t *testing.T) {
	r := reqlog.New(3)
	r.Add(reqlog.Entry{ID: "a"})
	got := r.Entries()
	got[0].ID = "mutated"
	if again := r.Entries(); again[0].ID != "a" {
		t.Fatalf("Entries 未返回副本，缓冲被外部改动：%+v", again)
	}
}
