// Package httpx 是 HTTP 响应装配的公共入口。
//
// 它只做一件事：把「本项目的编码纪律」固定下来 —— 所有响应体都走
// models.MarshalNoHTMLEscape（不转义 HTML 字符、不转义非 ASCII），
// 与靶机 Python `json.dumps(..., ensure_ascii=False)` 同形。
//
// 为什么必须集中：A2 已证明「在 MarshalJSON 里想办法」无效 —— 外层
// json.Marshal 会对返回值再压一遍转义。所以纪律必须落在**唯一出口**上，
// 而不是散在各 handler 里。依据：docs/contract/store/observations.md #21。
package httpx

import (
	"net/http"

	"github.com/LinYuanNull/zcode2api-go/internal/models"
)

// ContentTypeJSON 与靶机响应头逐字一致（A1 样本里没有 charset 参数）。
const ContentTypeJSON = "application/json"

// WriteRaw 写出已编码好的字节。
func WriteRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", ContentTypeJSON)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// WriteJSON 用保真编码写出 v；编码失败时退化为 500 + detail（编码失败意味着
// 响应体构造有 bug，不能静默返回空体）。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	b, err := models.MarshalNoHTMLEscape(v)
	if err != nil {
		WriteRaw(w, http.StatusInternalServerError,
			[]byte(`{"detail":"响应体编码失败"}`))
		return
	}
	WriteRaw(w, status, b)
}

// Detail 是 admin 域的错误体形态 `{"detail": "..."}`。
//
// 依据：docs/contract/admin/ 的**全部** admin 错误分支（400/401/404）都是这一形态。
// 注意 gateway 域用的是 `{"error":{"message","type"}}`，两套不可混用
// （见 SPEC.md 第三部分「错误体形态」）。
type Detail struct {
	Detail string `json:"detail"`
}

// WriteDetail 写出 admin 域错误体。
func WriteDetail(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, Detail{Detail: msg})
}
