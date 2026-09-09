package runtime

import (
	_ "embed"
	"net/http"
)

//go:embed landing.html
var landingHTML []byte

// NewLandingHandler 返回站点根路径（/）的 landing 页：纯静态自包含 HTML
// （无脚本、无外部资源，品牌 SVG 内联），页面内容随二进制发布。
// 仅精确匹配 "/"，API 与其余路径的 404 语义不受影响。
func NewLandingHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		setLandingSecurityHeaders(w.Header())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// no-cache：页面更新随新版本二进制发布，禁止中间层长期缓存。
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(landingHTML)
		}
	})
}

// setLandingSecurityHeaders 与 Console SPA 同款加固策略：landing 无脚本，
// 内联样式（自包含单文件）依赖 style-src 'unsafe-inline'。
func setLandingSecurityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:")
}
