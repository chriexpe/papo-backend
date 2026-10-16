package handlers

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"papo/internal/middleware"
	"testing"
)

// TestPublicAvatarOpaqueOrigin garante que a imagem pública carrega com CORS global sem credenciais.
func TestPublicAvatarOpaqueOrigin(t *testing.T) {
	e := newApp()
	e.Use(middleware.CORS([]string{"https://client.example"}))
	id, token := registerAndLogin(t, e)
	data, err := os.ReadFile("../../../examples/plugins/reference/icon.png")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"avatar": base64.StdEncoding.EncodeToString(data), "avatar_format": "PNG"})
	rec := do(t, e, "PUT", "/users/"+id+"/avatar", body, authCookie(token))
	if rec.Code != 200 && rec.Code != 204 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	for _, origin := range []string{"null", "https://client.example"} {
		req := httptest.NewRequest(http.MethodGet, "/users/"+id+"/avatar", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Cookie", "Auth=ignorado")
		out := httptest.NewRecorder()
		e.ServeHTTP(out, req)
		if out.Code != 200 || out.Header().Get("Cross-Origin-Resource-Policy") != "cross-origin" || out.Header().Get("Access-Control-Allow-Origin") != "*" || out.Header().Get("Access-Control-Allow-Credentials") != "" || out.Header().Get("Set-Cookie") != "" {
			t.Fatal(origin, out.Code, out.Header())
		}
	}
}
