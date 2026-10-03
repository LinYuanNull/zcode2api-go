package models

import "crypto/rand"

// NewUUID4 生成一个 uuid4 形态的字符串。
//
// 靶机的 `device_mid` 与 `install_id` 都是 uuid4 形态（observations.md #3 / #2）。
// 这里手写而不引第三方库：整个项目对 uuid 的需求只有「生成」，没有解析与比较。
func NewUUID4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在正常系统上不会失败；真失败也没有合理的降级（可预测的
		// 设备指纹比报错更糟），所以直接 panic。
		panic("models: 无法读取随机源: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	const hex = "0123456789abcdef"
	out := make([]byte, 36)
	i := 0
	for j, v := range b {
		if j == 4 || j == 6 || j == 8 || j == 10 {
			out[i] = '-'
			i++
		}
		out[i] = hex[v>>4]
		out[i+1] = hex[v&0x0f]
		i += 2
	}
	return string(out)
}
