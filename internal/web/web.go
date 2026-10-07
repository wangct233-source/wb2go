package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 打赏码图片也内嵌：单文件二进制是本项目的核心卖点之一，
// 不能因为两张图就要求用户额外拷目录。
// 已压成 WebP（209KB → 38KB），img-src 'self' 允许同源加载。
//
//go:embed assets/index.html assets/app.css assets/app.js
//go:embed assets/reward-qr.webp assets/reward-alipay.webp
var raw embed.FS

// assets 是去掉 assets/ 前缀后的文件系统。
var (
	once    sync.Once
	assets  fs.FS
	initErr error
)

func initAssets() {
	once.Do(func() {
		sub, err := fs.Sub(raw, "assets")
		if err != nil {
			initErr = err
			return
		}
		assets = sub
	})
}

// assetsFS 返回嵌入的前端资源。
func assetsFS() (fs.FS, error) {
	initAssets()
	return assets, initErr
}

// Register 把面板挂到 mux 上。
//
// 路由设计：
//
//	GET /              → 渲染面板首页（不做跳转，少一次往返）
//	GET /panel/        → 同上
//	GET /panel/app.css → 样式
//	GET /panel/app.js  → 脚本
//	GET /favicon.ico  → 204，避免浏览器无谓的 404 请求
//
// 静态资源走 go:embed 而不是从磁盘读：单文件二进制拷到任何机器都能直接跑，
// 不需要带着一整个 web 目录走。
func Register(mux *http.ServeMux, guard func(http.Handler) http.Handler) {
	f, err := assetsFS()
	if err != nil {
		panic("内置前端资源不可用: " + err.Error())
	}

	handler := guard(securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servePanel(w, r, f)
	})))

	mux.Handle("/panel/", handler)
	// 根路径直接渲染面板，而不是 302 跳 /panel/：
	// 用户从启动日志里复制到的地址通常就是根路径，少一次跳转更少一次困惑。
	mux.Handle("/", handler)

	// 浏览器总会自动请求 favicon，没有它每次打开面板都有一条 404 日志。
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}

// servePanel 处理面板路由。
func servePanel(w http.ResponseWriter, r *http.Request, f fs.FS) {
	rel := strings.TrimPrefix(r.URL.Path, "/panel")
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || !strings.Contains(rel, ".") {
		// 根路径与无扩展名的路径都返回首页，
		// 这样 /panel 与 /panel/ 都能打开（少一个末尾斜杠就 404 很别扭）
		rel = "index.html"
	}
	data, err := fs.ReadFile(f, rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(rel, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(rel, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(rel, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case strings.HasSuffix(rel, ".webp"):
		w.Header().Set("Content-Type", "image/webp")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	// 前端资源极少变动，给短缓存即可；改版后用户刷新就拿到新的
	w.Header().Set("Cache-Control", "public, max-age=300")
	http.ServeContent(w, r, rel, time.Time{}, strings.NewReader(string(data)))
}

// securityHeaders 给面板响应加上安全头。
//
// CSP 用 default-src 'none' 收紧到最小：面板只需要同源的样式与脚本，
// 不加载任何外部资源。这样即使页面里被注入标签，也没有外联能力。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		next.ServeHTTP(w, r)
	})
}
