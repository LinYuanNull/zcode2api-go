// Package pages 管理面板静态页面（内嵌前端资源）。
//
// A3 落地的是**面板宿主**：把一组静态资源按靶机的 URL 形状对外提供。
// 路由与响应头都是从运行中的靶机**实测**出来的（见下），不是猜的：
//
//	GET /              → 307 Location: /admin
//	GET /admin         → 307 Location: /admin/login
//	GET /admin/        → 307 Location: /admin
//	GET /admin/{page}  → {Dir}/admin/{page}.html，200 text/html; charset=utf-8
//	                     + Cache-Control: no-store；页面里的 {{APP_VERSION}} 会被替换成版本号
//	GET /static/{path} → {Dir}/{path}，按扩展名给 Content-Type
//	                     （实测：css → text/css; charset=utf-8，js → text/javascript; charset=utf-8）
//	其它               → 404 `{"detail":"Not Found"}`
//
// **上游前端不进本仓库**（许可纪律，见 README/PROVENANCE）：`Dir` 由使用者
// 用 `--panel-dir` 指向本地目录，A3 的验收就是把本机上游 `frontend/` 指过来。
// 未配置时给出一个明确说明的占位页，而不是空白页或 404。
package pages

import (
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/LinYuanNull/zcode2api-go/internal/httpx"
)

// Handler 是面板宿主。
type Handler struct {
	// Dir 是面板资源根目录（其下应有 admin/ css/ js/）。为空表示未配置。
	Dir string

	// Version 用于替换页面里的 {{APP_VERSION}}。
	Version string
}

// ServeHTTP 实现 http.Handler。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		httpx.WriteDetail(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	switch p := path.Clean(r.URL.Path); {
	case p == "/" || p == "/admin/":
		http.Redirect(w, r, "/admin", http.StatusTemporaryRedirect)
	case p == "/admin":
		http.Redirect(w, r, "/admin/login", http.StatusTemporaryRedirect)
	case strings.HasPrefix(p, "/static/"):
		h.serveStatic(w, r, strings.TrimPrefix(p, "/static/"))
	case strings.HasPrefix(p, "/admin/"):
		h.servePage(w, r, strings.TrimPrefix(p, "/admin/"))
	default:
		httpx.WriteDetail(w, http.StatusNotFound, "Not Found")
	}
}

// servePage 提供 /admin/{page}（无扩展名的页面名）。
func (h *Handler) servePage(w http.ResponseWriter, r *http.Request, page string) {
	// 只有「无扩展名的单段」才是页面；`/admin/accounts.html` 在靶机上是 404（实测）。
	if page == "" || strings.Contains(page, ".") || strings.Contains(page, "/") {
		httpx.WriteDetail(w, http.StatusNotFound, "Not Found")
		return
	}
	if h.Dir == "" {
		h.servePlaceholder(w, page)
		return
	}
	full, ok := h.resolve(filepath.Join("admin", page+".html"))
	if !ok {
		httpx.WriteDetail(w, http.StatusNotFound, "Not Found")
		return
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		httpx.WriteDetail(w, http.StatusNotFound, "Not Found")
		return
	}
	// 靶机对页面做一次版本占位符替换（实测：{{APP_VERSION}} → 2.6.3），
	// 且带 Cache-Control: no-store。
	body := strings.ReplaceAll(string(raw), "{{APP_VERSION}}", h.Version)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte(body))
	}
}

// serveStatic 提供 /static/{path}。
func (h *Handler) serveStatic(w http.ResponseWriter, r *http.Request, rel string) {
	if h.Dir == "" {
		httpx.WriteDetail(w, http.StatusNotFound, "Not Found")
		return
	}
	full, ok := h.resolve(rel)
	if !ok {
		httpx.WriteDetail(w, http.StatusNotFound, "Not Found")
		return
	}
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		httpx.WriteDetail(w, http.StatusNotFound, "Not Found")
		return
	}
	if ct := contentType(full); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeFile(w, r, full)
}

// resolve 把相对路径解析到 Dir 之内，拒绝目录穿越。
//
// 必须自己判：`http.ServeFile` 会拦 `..`，但 `os.ReadFile`（页面走这条）不会。
func (h *Handler) resolve(rel string) (string, bool) {
	rel = path.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))
	if rel == "/" || strings.HasPrefix(rel, "/..") {
		return "", false
	}
	full := filepath.Join(h.Dir, filepath.FromSlash(strings.TrimPrefix(rel, "/")))
	// 双保险：解析后的绝对路径必须仍在 Dir 之内。
	absDir, err1 := filepath.Abs(h.Dir)
	absFull, err2 := filepath.Abs(full)
	if err1 != nil || err2 != nil {
		return "", false
	}
	if absFull != absDir && !strings.HasPrefix(absFull, absDir+string(filepath.Separator)) {
		return "", false
	}
	return full, true
}

func contentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	case ".json":
		return "application/json"
	}
	return mime.TypeByExtension(filepath.Ext(name))
}

// servePlaceholder 在未配置面板目录时给出的说明页。
//
// 刻意不做成「空白页」：面板缺失是部署问题，应当一眼看得出来。
func (h *Handler) servePlaceholder(w http.ResponseWriter, page string) {
	body := `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>面板未配置</title>
<style>body{font:14px/1.7 system-ui,sans-serif;max-width:44rem;margin:4rem auto;padding:0 1.2rem;color:#222}
code{background:#f2f2f2;padding:.1rem .35rem;border-radius:4px}</style></head>
<body><h1>面板未配置</h1>
<p>本进程没有加载管理面板的静态资源。请用 <code>--panel-dir</code>（或环境变量
<code>ZCODE_PANEL_DIR</code>）指向面板资源目录；该目录下应有
<code>admin/</code>、<code>css/</code>、<code>js/</code> 三部分。</p>
<p>管理 API 本身不受影响：<code>/admin/api/*</code> 已可用（请求 <code>` + page + `</code> 对应的页面时看到的就是本页）。</p>
<p>接口自检：<code>GET /meta</code>、<code>GET /admin/api/verify</code>（需 <code>Authorization: Bearer &lt;后台密码&gt;</code>）。</p>
</body></html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}
