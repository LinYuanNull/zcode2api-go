package httpx_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LinYuanNull/zcode2api-go/internal/httpx"
)

// 保真编码是**唯一出口**纪律的判据：靶机是 Python `json.dumps(..., ensure_ascii=False)`，
// 它不转义 `< > &`，也不把中文转成 \uXXXX。Go 的 encoding/json 默认两样都做，
// 所以一旦有人绕过 httpx 直接用 json.Marshal，响应就会与靶机不同形。
func TestWriteJSONKeepsHTMLAndNonASCII(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.WriteJSON(rec, http.StatusOK, map[string]string{
		"msg": "a<b>&c 中文",
	})
	if got, want := rec.Body.String(), `{"msg":"a<b>&c 中文"}`; got != want {
		t.Fatalf("响应体被转义了：\n got=%q\nwant=%q", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type 应与靶机逐字一致（无 charset）：%q", ct)
	}
}

// 空容器必须是 `[]` / `{}`，不能是 `null` —— 面板对 `null` 直接 `.map` 会抛。
func TestWriteJSONEmptyContainers(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.WriteJSON(rec, http.StatusOK, struct {
		A []string          `json:"a"`
		B map[string]string `json:"b"`
	}{A: []string{}, B: map[string]string{}})
	if got, want := rec.Body.String(), `{"a":[],"b":{}}`; got != want {
		t.Fatalf("空容器形态不符：got=%q want=%q", got, want)
	}
}

// 键顺序 = struct 字段声明序（Go 保序序列化），这是「键顺序也是契约」的实现基础。
func TestWriteJSONPreservesFieldOrder(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.WriteJSON(rec, http.StatusOK, struct {
		Zeta  int `json:"zeta"`
		Alpha int `json:"alpha"`
	}{1, 2})
	if got, want := rec.Body.String(), `{"zeta":1,"alpha":2}`; got != want {
		t.Fatalf("字段序被改动：got=%q want=%q", got, want)
	}
}

// admin 域错误体是 `{"detail":"..."}`（紧凑、无空格），与 FastAPI 一致。
func TestWriteDetailShape(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.WriteDetail(rec, http.StatusNotFound, "Not Found")
	if got, want := rec.Body.String(), `{"detail":"Not Found"}`; got != want {
		t.Fatalf("admin 错误体形态不符：got=%q want=%q", got, want)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码不符：%d", rec.Code)
	}
	var back httpx.Detail
	if err := json.Unmarshal(rec.Body.Bytes(), &back); err != nil || back.Detail != "Not Found" {
		t.Fatalf("错误体不可解析：%v %+v", err, back)
	}
}

// WriteRaw 用于已经编码好的字节（例如把样本原文原样写出），不应再被加工。
func TestWriteRawPassesBytesThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	raw := []byte(`{"a": 1}`) // 带空格：原样保留，证明没有二次编码
	httpx.WriteRaw(rec, http.StatusTeapot, raw)
	if rec.Code != http.StatusTeapot || rec.Body.String() != string(raw) {
		t.Fatalf("WriteRaw 不应加工字节：%d %q", rec.Code, rec.Body.String())
	}
}
