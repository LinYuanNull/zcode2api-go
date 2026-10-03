package store

import (
	"encoding/json"
	"strconv"

	"github.com/LinYuanNull/zcode2api-go/internal/models"
)

// itoa 写 meta 表用：设置项在 settings 里是 int64，落库统一成十进制字符串。
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// marshalAccount 走 models 的**保真编码**入口（不转义 HTML 字符），
// 与靶机 Python `json.dumps(..., ensure_ascii=False)` 同形。
// 直接用 json.Marshal 会把 `<` 写成 `\u003c`，落盘 blob 就与靶机不同形了。
func marshalAccount(a models.Account) ([]byte, error) { return models.MarshalAccount(a) }

func unmarshalAccount(raw string, a *models.Account) error {
	return json.Unmarshal([]byte(raw), a)
}
