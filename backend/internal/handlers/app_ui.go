package handlers

import (
	"crypto/sha256"
	"fmt"
	"github.com/labstack/echo/v4"
	"mime"
	"net/http"
	"os"
	"papo/internal/services"
	"path"
	"path/filepath"
	"strings"
)

// AppUIHandler serve somente assets públicos estáticos: nunca lê sessão ou cookies.
// os.Root confina o caminho; symlinks são recusados mesmo se apontarem para dentro de ui/.
func AppUIHandler(baseURL string, c echo.Context) error {
	notFound := func() error { return c.NoContent(http.StatusNotFound) }
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Access-Control-Allow-Origin", "*")
	c.Response().Header().Del("Access-Control-Allow-Credentials")
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	c.Response().Header().Set("Referrer-Policy", "no-referrer")
	c.Response().Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
	c.Response().Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-forms; default-src 'self'")
	b, err := services.ResolvePluginSurfaceBundle(c.Request().Context(), c.Param("app_id"))
	if err != nil || b.Directory == "" {
		return notFound()
	}
	relative := c.Param("*")
	if relative == "" || strings.ContainsAny(relative, "\\\x00%") || path.Clean(relative) != relative || strings.HasPrefix(relative, "/") || relative == ".." || strings.HasPrefix(relative, "../") {
		return notFound()
	}
	extension := strings.ToLower(filepath.Ext(relative))
	switch extension {
	case ".html", ".css", ".js", ".mjs", ".json", ".png", ".jpg", ".jpeg", ".webp", ".gif", ".svg", ".ico", ".woff", ".woff2", ".ttf", ".otf", ".txt", ".wasm":
	default:
		return notFound()
	}
	dir := filepath.Join(b.Directory, "ui")
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return notFound()
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return notFound()
	}
	defer root.Close()
	parts := strings.Split(relative, "/")
	for i := range parts {
		info, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return notFound()
		}
	}
	file, err := root.Open(relative)
	if err != nil {
		return notFound()
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return notFound()
	}
	csp, err := services.PluginUICSP(c.Request().Context(), b, baseURL)
	if err != nil || b.Digest == "" {
		return notFound()
	}
	headers := c.Response().Header()
	headers.Set("Content-Security-Policy", csp)
	headers.Set("Cache-Control", "public, max-age=0, must-revalidate")
	headers.Set("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(b.Digest+"\x00"+relative))))
	if contentType := mime.TypeByExtension(extension); contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	http.ServeContent(c.Response(), c.Request(), relative, info.ModTime(), file)
	return nil
}
