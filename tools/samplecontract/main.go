// Command samplecontract 对着本机运行的上游靶机（dengyie/zcode2api）逐路由发送
// 真实请求，把观察到的 HTTP 行为（状态码 / 响应头 / 响应体）按结构清洗后写入
// docs/contract/，作为本仓库实现与验收的判据来源。
//
// 它是**开发工具**，不是发布产物的一部分，也不随包分发。样本规范见
// docs/contract/README.md，依据登记见 PROVENANCE.md。
//
// 用法：
//
//	go run ./tools/samplecontract -base http://127.0.0.1:3000 -admin-key 1234 -out docs/contract
//
// 清洗规则（只保留结构）：
//   - 按字段名脱敏：token/jwt/secret/api_key/admin_key/gateway_key/cookie/
//     authorization/device_mid/install_id/验证码参数 → 类型占位符。
//   - 按取值脱敏：邮箱、手机号、UUID、JWT 形态的字符串 → 类型占位符。
//   - 响应头只保留 Content-Type。
//   - 环境相关的标量（epoch 时间戳、计数）保留原值，由每条样本的 notes 说明。
//   - **保留原始键顺序**：清洗走流式 Token（`sanitizeOrdered`），不经过
//     `map[string]any`。键顺序是可观测契约的一部分（见 docs/contract/README.md）。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const maxBody = 64 << 10

// ── 样本结构（对齐 docs/contract/README.md）─────────────────────────────────

type sample struct {
	Route    string       `json:"route"`
	Method   string       `json:"method"`
	Request  requestView  `json:"request"`
	Response responseView `json:"response"`
	Notes    string       `json:"notes"`
}

type requestView struct {
	Query   map[string]string `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

type responseView struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

// ── 采样器 ──────────────────────────────────────────────────────────────────

type sampler struct {
	base     string
	adminKey string
	client   *http.Client
	outDir   string
	rows     []row
	// redactIDs 收集靶机上真实存在的账号 id（形如 <name>-<8hex>，非 UUID），
	// 落盘前在整份样本里做精确替换 —— 账号 id 属「账号标识」，按规范必须脱敏。
	redactIDs []string
}

type row struct {
	route, method, file string
	status              int
}

func main() {
	base := flag.String("base", "http://127.0.0.1:3000", "靶机基址")
	adminKey := flag.String("admin-key", "1234", "靶机后台管理密钥")
	outDir := flag.String("out", "docs/contract", "样本输出目录（相对仓库根）")
	flag.Parse()

	s := &sampler{
		base:     strings.TrimRight(*base, "/"),
		adminKey: *adminKey,
		client:   &http.Client{Timeout: 30 * time.Second},
		outDir:   *outDir,
	}

	if err := s.run(); err != nil {
		fmt.Fprintln(os.Stderr, "采样失败:", err)
		os.Exit(1)
	}
	s.summary()
}

// ── 主流程：按「先建后删」的执行顺序覆盖每个路由的成功与错误分支 ─────────────

func (s *sampler) run() error {
	// 01 鉴权探针
	if _, err := s.record("admin", "01", "verify-ok", "GET", "/admin/api/verify", nil, s.adminKey, nil,
		"正确密钥。返回体仅 {status:ok}。"); err != nil {
		return err
	}
	if _, err := s.record("admin", "01", "verify-unauthorized", "GET", "/admin/api/verify", nil, "",
		nil, "缺凭证分支：无 Authorization 头 → 401，错误体 {detail:…}（FastAPI 默认 HTTPException 形态）。"); err != nil {
		return err
	}

	// 02 账号列表（空态）
	if _, err := s.record("admin", "02", "accounts-empty", "GET", "/admin/api/accounts", nil, s.adminKey,
		nil, "空账号池。stats 各计数为 0；providers 为固定枚举 [zai bigmodel]；ts 为 epoch 秒（随环境变化）。"); err != nil {
		return err
	}

	// 03 状态概览
	if _, err := s.record("admin", "03", "status", "GET", "/admin/api/status", nil, s.adminKey,
		nil, "gateway_key_set 反映网关 Key 是否配置；quota_pool 为各 provider 可选用账号数。"); err != nil {
		return err
	}

	// 04 新增账号：错误分支 + 成功分支（成功分支的 id 供后续 account-scoped 路由复用）
	if _, err := s.record("admin", "04", "accounts-add-badprovider", "POST", "/admin/api/accounts", nil,
		s.adminKey, map[string]any{"provider": "nope", "tokens": []string{"x"}},
		"provider 不在枚举内 → 400 {detail:不支持的 provider}。"); err != nil {
		return err
	}
	if _, err := s.record("admin", "04", "accounts-add-notokens", "POST", "/admin/api/accounts", nil,
		s.adminKey, map[string]any{"provider": "zai", "tokens": []string{}},
		"tokens 为空 → 400 {detail:请输入至少一个 Token / API Key}。"); err != nil {
		return err
	}
	addRes, err := s.record("admin", "04", "accounts-add-ok", "POST", "/admin/api/accounts", nil,
		s.adminKey, map[string]any{"provider": "zai", "tokens": []string{"contract-sample-token"}, "name": "contract-sample"},
		"成功新增。返回 {count, ids[]}；ids 为服务端生成的账号 id，形态为 <slug>-<8hex>（样本中已脱敏）。")
	if err != nil {
		return err
	}
	accID := firstID(addRes.body)
	if err := s.collectIDs(); err != nil {
		return err
	}

	// 02' 账号列表（一条）——展示 public_view 的完整字段集
	if _, err := s.record("admin", "02", "accounts-one", "GET", "/admin/api/accounts", nil, s.adminKey,
		nil, "含一条 apiKey 账号。public_view 字段集：id/name/provider/mode/token_masked/enabled/status/"+
			"quota/plan/plans/use_count/fail_count/risk_strikes/recent_results/last_used_at/"+
			"last_checked_at/cooling_until/last_error/created_at/fingerprint/install_id/installed_at。"+
			"其中 *_at 为 epoch 秒或 null，fingerprint 为设备档案对象（device_mid 已脱敏）。"); err != nil {
		return err
	}

	// 06 编辑账号
	if _, err := s.record("admin", "06", "account-edit-ok", "PUT", "/admin/api/accounts/"+accID, nil,
		s.adminKey, map[string]any{"name": "contract-renamed"},
		"成功编辑：改 name。返回 {ok:true}。路径参数为账号 UUID（样本中已脱敏）。"); err != nil {
		return err
	}
	if _, err := s.record("admin", "06", "account-edit-404", "PUT", "/admin/api/accounts/00000000-0000-0000-0000-000000000000",
		nil, s.adminKey, map[string]any{"name": "x"},
		"账号不存在 → 404 {detail:账号不存在}。"); err != nil {
		return err
	}

	// 07 启用 / 禁用
	if _, err := s.record("admin", "07", "account-enabled-ok", "POST", "/admin/api/accounts/"+accID+"/enabled",
		nil, s.adminKey, map[string]any{"enabled": false},
		"成功：enabled=false 禁用该账号。返回 {ok:true}。"); err != nil {
		return err
	}
	if _, err := s.record("admin", "07", "account-enabled-404", "POST",
		"/admin/api/accounts/00000000-0000-0000-0000-000000000000/enabled", nil, s.adminKey,
		map[string]any{"enabled": true}, "账号不存在 → 404 {detail:账号不存在}。"); err != nil {
		return err
	}

	// 08 指纹换发
	if _, err := s.record("admin", "08", "account-fingerprint-ok", "POST",
		"/admin/api/accounts/"+accID+"/fingerprint/rotate", nil, s.adminKey, nil,
		"成功：换发设备档案。返回 {ok:true, fingerprint:{platform/arch/os_version/language/"+
			"timezone/screen/device_mid}}；device_mid 已脱敏，其余为枚举字符串。"); err != nil {
		return err
	}
	if _, err := s.record("admin", "08", "account-fingerprint-404", "POST",
		"/admin/api/accounts/00000000-0000-0000-0000-000000000000/fingerprint/rotate", nil, s.adminKey,
		nil, "账号不存在 → 404 {detail:账号不存在}。"); err != nil {
		return err
	}

	// 09 批量刷新额度
	if _, err := s.record("admin", "09", "accounts-refresh-all", "POST", "/admin/api/accounts/refresh",
		nil, s.adminKey, map[string]any{"all": true},
		"all=true。返回 {summary, count, skipped_cooling, skipped_invalid}；"+
			"空池 / 无 JWT 账号时 count=0，summary 为空映射。"); err != nil {
		return err
	}

	// 10 单账号刷新额度
	if _, err := s.record("admin", "10", "account-refresh-nonjwt", "POST",
		"/admin/api/accounts/"+accID+"/refresh", nil, s.adminKey, nil,
		"非 JWT（apiKey）账号：不查上游，返回 {ok:false, message:仅 Coding Plan (JWT) 账号支持额度查询}。"); err != nil {
		return err
	}
	if _, err := s.record("admin", "10", "account-refresh-404", "POST",
		"/admin/api/accounts/00000000-0000-0000-0000-000000000000/refresh", nil, s.adminKey, nil,
		"账号不存在 → 404 {detail:账号不存在}。"); err != nil {
		return err
	}

	// 11 OAuth 登录发起
	if _, err := s.record("admin", "11", "login-start", "POST", "/admin/api/login/start", nil,
		s.adminKey, map[string]any{"label": "contract"},
		"发起 Z.AI OAuth。成功返回 {flow_id, authorize_url, expires_in}；"+
			"flow_id 为服务端会话标识（32 位 hex，样本中已脱敏），authorize_url 为上游授权地址"+
			"（含上游域名，非本机，样本中已脱敏）。上游不可达时 → 502 {detail:登录初始化失败: …}"+
			"（也是本路由的错误分支形态）。"); err != nil {
		return err
	}

	// 12 轮询登录状态（未知 flow → expired，而非 404）
	if _, err := s.record("admin", "12", "login-poll-unknown", "GET",
		"/admin/api/login/poll/00000000-0000-0000-0000-000000000000", nil, s.adminKey, nil,
		"未知 flow_id 一律返回 {status:expired}（HTTP 200），不返回 404。"+
			"已知 flow 的其它取值：pending / ready / failed（failed 附 message）。"); err != nil {
		return err
	}

	// 13 领取预览（无 JWT 账号 → 空数组）
	if _, err := s.record("admin", "13", "claim-preview-empty", "GET", "/admin/api/claim/preview", nil,
		s.adminKey, nil, "无 JWT 账号时 preview 为空数组。有账号时每项含 {account_id, account_name, "+
			"plans[], error, activated, activation_error}。"); err != nil {
		return err
	}

	// 14 领取（无候选账号 → 空 outcomes）
	if _, err := s.record("admin", "14", "claim-empty", "POST", "/admin/api/claim", nil, s.adminKey,
		map[string]any{}, "无 JWT 候选账号 → {outcomes:[], summary:{ok:0, fail:0}}。"+
			"有账号时 outcomes[] 每项含 {account_id, account_name, ok, plan_name?, grants?, message?, code?, next_at?}。"); err != nil {
		return err
	}

	// 15 手动领取用的验证码配置
	if _, err := s.record("admin", "15", "claim-captcha-config", "GET", "/admin/api/claim/captcha-config",
		nil, s.adminKey, nil, "返回阿里验证码 SDK 初始化参数 {enabled, scene_id, region, prefix}；"+
			"取值来自上游验证码服务（随环境变化，可能为空串）。"); err != nil {
		return err
	}

	// 16 手动领取
	if _, err := s.record("admin", "16", "claim-manual-missing-id", "POST", "/admin/api/claim/manual", nil,
		s.adminKey, map[string]any{"captcha_verify_param": "x"},
		"缺 account_id → 400 {detail:缺少 account_id}。"); err != nil {
		return err
	}
	if _, err := s.record("admin", "16", "claim-manual-404", "POST", "/admin/api/claim/manual", nil,
		s.adminKey, map[string]any{"account_id": "00000000-0000-0000-0000-000000000000", "captcha_verify_param": "x"},
		"账号非 JWT 或不存在 → 404 {detail:JWT 账号不存在}。"); err != nil {
		return err
	}

	// 17 读取设置
	if _, err := s.record("admin", "17", "settings-get", "GET", "/admin/api/settings", nil, s.adminKey,
		nil, "返回 {admin_key_set, admin_key_masked, admin_key_is_default, gateway_key_set, "+
			"gateway_key_masked, quota_refresh_interval, account_concurrency, claim_round_interval}。"+
			"掩码字段形如 'ab12…'（样本中已脱敏为占位符），布尔与整数为结构性取值。"); err != nil {
		return err
	}

	// 18 写入设置：错误分支 + 成功分支（顺带把网关 Key 置为已知值，供 23–25 采鉴权分支）
	if _, err := s.record("admin", "18", "settings-put-empty-key", "PUT", "/admin/api/settings", nil,
		s.adminKey, map[string]any{"admin_key": ""},
		"admin_key 为空 → 400 {detail:后台密钥不能为空}。"); err != nil {
		return err
	}
	if _, err := s.record("admin", "18", "settings-put-ok", "PUT", "/admin/api/settings", nil, s.adminKey,
		map[string]any{"gateway_key": "contract-gw-key", "quota_refresh_interval": 1800,
			"account_concurrency": 2, "claim_round_interval": 0},
		"成功写入。返回 {ok:true}。本步顺带把 gateway_key 置为已知值，供网关路由采鉴权分支。"); err != nil {
		return err
	}

	// 19 导出账号
	if _, err := s.record("admin", "19", "export", "GET", "/admin/api/export", nil, s.adminKey, nil,
		"返回 {version, exported_at, providers:{<provider>:[{name, mode, secret}]}}；"+
			"secret 为**明文**凭据（样本中已整体脱敏为占位符）。exported_at 为 epoch 秒。"); err != nil {
		return err
	}

	// 20 导入账号
	if _, err := s.record("admin", "20", "import-ok", "POST", "/admin/api/import", nil, s.adminKey,
		map[string]any{"providers": map[string]any{"zai": []any{
			map[string]any{"name": "contract-import", "secret": "contract-import-secret"}}}},
		"成功导入一条。返回 {count:1}。secret 为明文凭据（样本中已脱敏）。"); err != nil {
		return err
	}
	if err := s.collectIDs(); err != nil {
		return err
	}

	// 21 请求监控
	if _, err := s.record("admin", "21", "monitoring", "GET", "/admin/api/monitoring", nil, s.adminKey,
		nil, "返回 {entries:[…], keep:<容量>}。entries 为内存环形日志，含本会话已发生的网关请求"+
			"（id/model/stream/prompt/status/…）；重启清零，entries 条数随环境变化。"); err != nil {
		return err
	}

	// 22 清空监控
	if _, err := s.record("admin", "22", "monitoring-clear", "POST", "/admin/api/monitoring/clear", nil,
		s.adminKey, nil, "清空内存环形日志，返回 {ok:true}。"); err != nil {
		return err
	}

	// 05 删除账号：成功 + 不存在
	delIDs := []string{}
	if accID != "" {
		delIDs = append(delIDs, accID)
	}
	if _, err := s.record("admin", "05", "accounts-delete-ok", "DELETE", "/admin/api/accounts", nil,
		s.adminKey, delIDs, "成功删除给定 ids（body 为字符串数组）。返回 {deleted:<实际删除数>}。"+
			"重复 id / 不存在的 id 不计入 deleted。"); err != nil {
		return err
	}
	if _, err := s.record("admin", "05", "accounts-delete-none", "DELETE", "/admin/api/accounts", nil,
		s.adminKey, []string{"00000000-0000-0000-0000-000000000000"},
		"全部 id 都不存在 → {deleted:0}（不报错）。"); err != nil {
		return err
	}

	// ── 网关路由（gateway_key 已在上一步置为 contract-gw-key）──────────────

	// 23 模型列表
	if _, err := s.record("gateway", "23", "models-noauth", "GET", "/v1/models", nil, "",
		nil, "已配置网关 Key 但请求不带凭证 → 401 {detail:缺少 API Key}。"); err != nil {
		return err
	}
	if _, err := s.record("gateway", "23", "models-wrongkey", "GET", "/v1/models", nil, "wrong-key",
		nil, "凭证错误 → 403 {detail:API Key 无效}。"); err != nil {
		return err
	}
	if _, err := s.record("gateway", "23", "models-ok", "GET", "/v1/models", nil, "contract-gw-key",
		nil, "成功。返回 Anthropic 风格 {object:list, data:[{id, type:model, display_name, created_at}]}；"+
			"模型表为编译期常量（app/constants.AVAILABLE_MODELS）。"); err != nil {
		return err
	}

	// 24 /v1/messages
	if _, err := s.record("gateway", "24", "messages-noauth", "POST", "/v1/messages", nil, "",
		map[string]any{"model": "glm-4.6", "max_tokens": 16, "messages": []any{}},
		"无凭证 → 401（与 23 同鉴权依赖）。"); err != nil {
		return err
	}
	if _, err := s.record("gateway", "24", "messages-badjson", "POST", "/v1/messages", nil,
		"contract-gw-key", rawJSON("not-json"),
		"请求体非合法 JSON → 400 {error:{message:请求体不是合法 JSON, type:invalid_request}}。"); err != nil {
		return err
	}
	if _, err := s.record("gateway", "24", "messages-noaccount", "POST", "/v1/messages", nil,
		"contract-gw-key", map[string]any{"model": "glm-4.6", "max_tokens": 16,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		"合法请求但账号池无可用账号 → 网关错误分支（具体形态以样本为准）。"); err != nil {
		return err
	}

	// 25 /v1/chat/completions
	if _, err := s.record("gateway", "25", "chat-noauth", "POST", "/v1/chat/completions", nil, "",
		map[string]any{"model": "glm-4.6", "messages": []any{}},
		"无凭证 → 401（与 23 同鉴权依赖）。"); err != nil {
		return err
	}
	if _, err := s.record("gateway", "25", "chat-badjson", "POST", "/v1/chat/completions", nil,
		"contract-gw-key", rawJSON("not-json"),
		"请求体非合法 JSON → 400 {error:{message:请求体不是合法 JSON, type:invalid_request_error}}。"); err != nil {
		return err
	}
	if _, err := s.record("gateway", "25", "chat-noaccount", "POST", "/v1/chat/completions", nil,
		"contract-gw-key", map[string]any{"model": "glm-4.6",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		"合法请求但账号池无可用账号 → 网关错误分支（具体形态以样本为准）。"); err != nil {
		return err
	}

	// 复原：把网关 Key 清空，避免样本采集对外部状态留痕
	if _, err := s.record("admin", "18", "settings-put-reset-gwkey", "PUT", "/admin/api/settings", nil,
		s.adminKey, map[string]any{"gateway_key": ""},
		"复原：把 gateway_key 置回空串（空 = 网关不校验）。返回 {ok:true}。"); err != nil {
		return err
	}

	return nil
}

// ── 请求 + 落盘 ────────────────────────────────────────────────────────────

type result struct {
	status int
	body   any
}

func (s *sampler) record(area, seq, slug, method, path string, query map[string]string,
	bearer string, body any, notes string) (*result, error) {

	full := s.base + path
	if len(query) > 0 {
		q := url.Values{}
		for k, v := range query {
			q.Set(k, v)
		}
		full += "?" + q.Encode()
	}

	var payload []byte
	reqHeaders := map[string]string{}
	if body != nil {
		switch v := body.(type) {
		case rawJSON:
			payload = []byte(v)
		default:
			// 注意：请求体是**采样器自己构造**的（`map[string]any` 字面量），
			// 所以样本里请求体的键顺序是 Go 编码 map 的字典序，属**工具产物**，
			// 不是契约。只有**响应体**的键顺序才是上游的可观测事实（走 sanitizeOrdered）。
			b, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("%s %s: 序列化请求体: %w", method, path, err)
			}
			payload = b
		}
		reqHeaders["Content-Type"] = "application/json"
	}
	if bearer != "" {
		reqHeaders["Authorization"] = "Bearer " + bearer
	}

	req, err := http.NewRequest(method, full, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for k, v := range reqHeaders {
		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))

	// 请求视图
	reqView := requestView{Query: map[string]string{}, Headers: map[string]string{}, Body: json.RawMessage("null")}
	for k, v := range query {
		reqView.Query[k] = redactString(v)
	}
	if _, ok := reqHeaders["Content-Type"]; ok {
		reqView.Headers["Content-Type"] = "application/json"
	}
	if bearer != "" {
		reqView.Headers["Authorization"] = "<redacted:bearer>"
	}
	if body != nil {
		if ob, err := sanitizeOrdered(payload, ""); err == nil {
			reqView.Body = ob
		} else {
			reqView.Body = mustJSON("<raw:non-json>")
		}
	}

	// 响应视图
	respView := responseView{Status: resp.StatusCode, Headers: map[string]string{}}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		respView.Headers["Content-Type"] = ct
	}
	decoded, ok := decodeJSON(raw)
	switch {
	case ok:
		// 按**原始键顺序**清洗 —— 顺序是契约的一部分（见 docs/contract/README.md）。
		ob, err := sanitizeOrdered(raw, "")
		if err != nil {
			return nil, fmt.Errorf("清洗响应体失败: %w", err)
		}
		respView.Body = ob
	case len(raw) == 0:
		respView.Body = json.RawMessage("null")
	default:
		respView.Body = mustJSON("<raw:non-json:" + fmt.Sprintf("%d bytes", len(raw)) + ">")
	}

	smp := sample{
		Route:    path,
		Method:   method,
		Request:  reqView,
		Response: respView,
		Notes:    notes,
	}
	out, err := encodeSample(smp)
	if err != nil {
		return nil, err
	}

	// 账号标识精确替换：把靶机上真实存在的账号 id 换成占位符（含 route 字段里的
	// 路径参数）。样本落盘后不得残留任何真实账号标识。
	if ok {
		s.harvestIDs(decoded) // 兜底：从响应体里捞出刚生成的账号 id（如新增账号的返回值）
	}
	for _, id := range s.redactIDs {
		if id != "" {
			out = bytes.ReplaceAll(out, []byte(id), []byte("<redacted:account-id>"))
		}
	}
	dir := filepath.Join(s.outDir, area)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s-%s.%s.json", seq, slug, method)
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, out, 0o644); err != nil {
		return nil, err
	}
	rel := filepath.ToSlash(file)
	s.rows = append(s.rows, row{route: path, method: method, file: rel, status: resp.StatusCode})
	fmt.Printf("  %-4d %-6s %-48s → %s\n", resp.StatusCode, method, path, rel)

	res := &result{status: resp.StatusCode}
	if ok {
		res.body = decoded
	}
	return res, nil
}

func (s *sampler) summary() {
	sort.Slice(s.rows, func(i, j int) bool { return s.rows[i].file < s.rows[j].file })
	fmt.Printf("\n共 %d 条样本\n", len(s.rows))
}

// collectIDs 拉取当前账号池，把真实账号 id 记入脱敏集合（供落盘前整体替换）。
func (s *sampler) collectIDs() error {
	req, err := http.NewRequest("GET", s.base+"/admin/api/accounts", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.adminKey)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	decoded, ok := decodeJSON(raw)
	if !ok {
		return nil
	}
	m, ok := decoded.(map[string]any)
	if !ok {
		return nil
	}
	list, _ := m["accounts"].([]any)
	for _, item := range list {
		if acc, ok := item.(map[string]any); ok {
			if id, ok := acc["id"].(string); ok && id != "" {
				s.redactIDs = append(s.redactIDs, id)
			}
		}
	}
	return nil
}

// harvestIDs 递归扫描响应体，把形如 <slug>-<8hex> 的账号 id 记入脱敏集合。
// 这是 collectIDs（走接口拉全量）之外的第二道保险：某些响应会直接返回刚生成的
// 账号 id（新增 / 编辑 / 刷新账号），此时接口快照尚未更新。
func (s *sampler) harvestIDs(v any) {
	switch val := v.(type) {
	case map[string]any:
		for _, vv := range val {
			s.harvestIDs(vv)
		}
	case []any:
		for _, vv := range val {
			s.harvestIDs(vv)
		}
	case string:
		if reAcctID.MatchString(val) && !s.hasID(val) {
			s.redactIDs = append(s.redactIDs, val)
		}
	}
}

func (s *sampler) hasID(id string) bool {
	for _, x := range s.redactIDs {
		if x == id {
			return true
		}
	}
	return false
}

// encodeSample 以 2 空格缩进输出样本，并关闭 HTML 转义 —— 否则脱敏占位符里的
// `<` `>` 会被写成 \u003c / \u003e，样本可读性变差。
//
// 为什么先紧凑编码再 `json.Indent`，而不是直接用 `Encoder.SetIndent`：
// 响应体是 `json.RawMessage`（为保住原始键顺序），而 `Encoder` 会把 `RawMessage`
// 当成不透明字节**紧凑**写出去，缩进就丢了。`json.Indent` 是纯空白格式化，
// 不重排键、不改写字符串，正好补上缩进。
func encodeSample(smp sample) ([]byte, error) {
	var compact bytes.Buffer
	enc := json.NewEncoder(&compact)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(smp); err != nil {
		return nil, err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, bytes.TrimRight(compact.Bytes(), "\n"), "", "  "); err != nil {
		return nil, err
	}
	pretty.WriteByte('\n')
	return pretty.Bytes(), nil
}

// ── 工具 ───────────────────────────────────────────────────────────────────

type rawJSON string

func decodeJSON(b []byte) (any, bool) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, false
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, false
	}
	return v, true
}

func firstID(body any) string {
	m, ok := body.(map[string]any)
	if !ok {
		return ""
	}
	ids, ok := m["ids"].([]any)
	if !ok || len(ids) == 0 {
		return ""
	}
	s, _ := ids[0].(string)
	return s
}

// ── 脱敏 ───────────────────────────────────────────────────────────────────

var sensitiveKeys = map[string]bool{
	"authorization": true, "cookie": true, "set-cookie": true,
	"token": true, "jwt": true, "jwt_token": true, "api_key": true, "apikey": true,
	"secret": true, "admin_key": true, "gateway_key": true, "app_key": true,
	"password": true, "access_token": true, "refresh_token": true,
	"captcha_verify_param": true, "verify_param": true,
	"device_mid": true, "install_id": true,
	"tokens": true, "flow_id": true,
}

var maskedKeys = map[string]bool{
	"token_masked": true, "admin_key_masked": true, "gateway_key_masked": true,
}

// urlKeys 承载可完成流程的授权地址（含上游会话标识），只保留「这是一个 URL」。
var urlKeys = map[string]bool{
	"authorize_url": true,
}

var (
	reEmail = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	reUUID  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reJWT   = regexp.MustCompile(`^[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+$`)
	rePhone = regexp.MustCompile(`^\+?\d{7,15}$`)
	// 32 位纯小写 hex：上游多种会话 / 设备标识的形态（如 OAuth flow_id、device_mid）。
	// 取值级兜底，防止新增字段漏配键规则。
	reHex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// 账号 id 形态：<slug>-<8 位 hex>（上游 _account_id 的生成规则）。
	reAcctID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}-[0-9a-f]{8}$`)
)

// sanitizeStringValue 对「位于 key 之下的字符串」施加脱敏规则。
//
// 规则按优先级：敏感键 → 掩码键 → URL 键 → 取值形态（邮箱 / 手机 / UUID / JWT / 32hex）。
func sanitizeStringValue(key, s string) string {
	lk := strings.ToLower(key)
	if sensitiveKeys[lk] {
		return "<redacted:string>"
	}
	if maskedKeys[lk] {
		return "<redacted:masked>"
	}
	if urlKeys[lk] {
		return "<redacted:url>"
	}
	return redactString(s)
}

// sanitizeOrdered 按**原始键顺序**清洗 JSON，返回紧凑的 JSON 字节。
//
// 为什么不能用 `json.Unmarshal` 到 `map[string]any` 再编码：Go 的 map 无序，
// `json.Marshal` 会把键**按字典序**输出 —— 样本就不再是「真实响应体」了。
// 键顺序是可观测契约（A2 已证明：落盘 `data` 与 `/admin/api/accounts` 的键序
// 都不是字典序，且被逐字节校验）。所以这里走流式 Token，逐个键照原序写回。
//
// `key` 是当前所处字段名，用于脱敏判定；数组元素**继承父键**，这样
// `tokens: ["…"]` 这类「敏感键 + 数组」的元素也会被脱敏，同时保住数组结构。
func sanitizeOrdered(raw []byte, key string) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var buf bytes.Buffer
	if err := writeSanitized(&buf, dec, key); err != nil {
		return nil, err
	}
	return json.RawMessage(buf.Bytes()), nil
}

func writeSanitized(buf *bytes.Buffer, dec *json.Decoder, key string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			buf.WriteByte('{')
			first := true
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				k, ok := kt.(string)
				if !ok {
					return fmt.Errorf("对象键不是字符串: %v", kt)
				}
				if !first {
					buf.WriteByte(',')
				}
				first = false
				buf.Write(mustJSON(k))
				buf.WriteByte(':')
				if err := writeSanitized(buf, dec, k); err != nil {
					return err
				}
			}
			end, err := dec.Token()
			if err != nil {
				return err
			}
			if end != json.Delim('}') {
				return fmt.Errorf("对象未正确闭合: %v", end)
			}
			buf.WriteByte('}')
		case '[':
			buf.WriteByte('[')
			first := true
			for dec.More() {
				if !first {
					buf.WriteByte(',')
				}
				first = false
				if err := writeSanitized(buf, dec, key); err != nil { // 键向下传播
					return err
				}
			}
			end, err := dec.Token()
			if err != nil {
				return err
			}
			if end != json.Delim(']') {
				return fmt.Errorf("数组未正确闭合: %v", end)
			}
			buf.WriteByte(']')
		default:
			return fmt.Errorf("意外的分隔符: %v", t)
		}
	case string:
		buf.Write(mustJSON(sanitizeStringValue(key, t)))
	case json.Number:
		// 原样输出，保住整数与高精度小数的字面形态（不经过 float64）。
		buf.WriteString(t.String())
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case nil:
		buf.WriteString("null")
	default:
		return fmt.Errorf("意外的 Token: %v", tok)
	}
	return nil
}

// mustJSON 编码一个 JSON 值，关闭 HTML 转义（否则脱敏占位符里的 < > 会变形）。
func mustJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic("samplecontract: 无法编码 JSON 字面量: " + err.Error())
	}
	b := buf.Bytes()
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	return b
}

func redactString(s string) string {
	switch {
	case reEmail.MatchString(s):
		return "<redacted:email>"
	case reUUID.MatchString(s):
		return "<redacted:uuid>"
	case reJWT.MatchString(s):
		return "<redacted:jwt>"
	case rePhone.MatchString(s):
		return "<redacted:phone>"
	case reHex32.MatchString(s):
		return "<redacted:hex32>"
	default:
		return s
	}
}
