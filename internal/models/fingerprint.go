package models

// Fingerprint 是账号的设备档案。
//
// **字段顺序即落盘顺序**（observations.md 第 2 节末段：platform, arch, os_version,
// language, timezone, screen, device_mid），不要按字典序重排。
//
// 取值池与平台相关性见 docs/contract/store/fingerprint-shape.json。
type Fingerprint struct {
	Platform  string `json:"platform"`
	Arch      string `json:"arch"`
	OSVersion string `json:"os_version"`
	Language  string `json:"language"`
	Timezone  string `json:"timezone"`
	Screen    string `json:"screen"`
	DeviceMID string `json:"device_mid"`
}

// FingerprintOf 从原始 JSON 解出设备档案。缺失字段留空字符串。
func FingerprintOf(raw []byte) (Fingerprint, error) {
	var f Fingerprint
	if err := decodeField(raw, &f); err != nil {
		return Fingerprint{}, err
	}
	return f, nil
}

// Bytes 按落盘顺序编码。
func (f Fingerprint) Bytes() ([]byte, error) { return marshalNoHTMLEscape(f) }
