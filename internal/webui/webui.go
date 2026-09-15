// Package webui 承载 /admin 的构建产物。
//
// 依据 ADR-4(V2.20):
//   - web/ 源码留在顶层,**构建产物拷入本包 dist/ 后编译进 netdisk 二进制**
//   - Nginx 不再托管静态资源(3.2 职责 2 已改)
//   - index.html 必须 no-cache;带内容哈希的 assets/* 长缓存 immutable(R-02)
//
// 注意:dist 目录必须存在(哪怕只有占位 index.html),否则 go:embed 会编译失败。
// 正式构建顺序由 scripts/gen.cmd 保证:先 pnpm build,再拷 dist,最后 go build。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/netdisk/netdisk/internal/apierr"
)

//go:embed all:dist
var distFS embed.FS

// Handler 返回某个 SPA 前缀(如 /admin/、/h5/)的处理器。
//
// 行为:
//  1. 命中真实文件 → 按扩展名给缓存头后返回
//  2. 未命中且不是 assets/ → 回退 index.html(SPA 路由)
//  3. assets/ 未命中 → 404(不把 HTML 当 JS 返回,避免诡异的 MIME 错误)
func Handler(prefix, site string, requestIDHeader bool) http.Handler {
	sub, err := fs.Sub(distFS, path.Join("dist", site))
	if err != nil {
		// 构建期已保证存在;运行时兜底为 503 而不是 panic
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apierr.Write(w, r, apierr.Internal(err))
		})
	}
	fileServer := http.FileServer(http.FS(sub))

	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		f, err := sub.Open("index.html")
		if err != nil {
			apierr.Write(w, r, apierr.NotFound("前端产物缺失: %s(请先执行 scripts/gen.cmd)", site))
			return
		}
		defer f.Close()
		stat, _ := f.Stat()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		http.ServeContent(w, r, "index.html", stat.ModTime(), f.(interface {
			Read([]byte) (int, error)
			Seek(int64, int) (int64, error)
		}))
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(prefix, "/"))
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			serveIndex(w, r)
			return
		}
		// 目录穿越防护
		if strings.Contains(rel, "..") {
			apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "非法路径"))
			return
		}
		if f, err := sub.Open(rel); err == nil {
			_ = f.Close()
			setCacheHeaders(w, rel)
			// 让 FileServer 处理 Range/If-Modified-Since
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/" + rel
			fileServer.ServeHTTP(w, r2)
			return
		}
		if strings.HasPrefix(rel, "assets/") {
			apierr.Write(w, r, apierr.NotFound("静态资源不存在"))
			return
		}
		serveIndex(w, r)
	})
}

// setCacheHeaders 实现 R-02。
func setCacheHeaders(w http.ResponseWriter, rel string) {
	switch {
	case strings.HasPrefix(rel, "assets/"):
		// Vite 产出的 assets 文件名带内容哈希 → 可永久缓存
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	case strings.HasSuffix(rel, ".html"):
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	default:
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	if ct := mimeOf(rel); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
}

func mimeOf(rel string) string {
	switch path.Ext(rel) {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json; charset=utf-8"
	case ".woff2":
		return "font/woff2"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	case ".html":
		return "text/html; charset=utf-8"
	default:
		return ""
	}
}
