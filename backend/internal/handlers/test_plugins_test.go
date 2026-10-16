package handlers

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/storage"
)

// TestPluginRoutes cobre consentimento real, auth, UI e remoção pelo roteador de produção.
func TestPluginRoutes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PLUGINS_DIR", dir)
	for _, id := range []string{"reference", "bluemap"} {
		src := filepath.Join("../../../examples/plugins", id)
		e := filepath.WalkDir(src, func(name string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			rel, _ := filepath.Rel(src, name)
			dst := filepath.Join(dir, id, rel)
			if d.IsDir() {
				return os.MkdirAll(dst, 0700)
			}
			data, e := os.ReadFile(name)
			if e != nil {
				return e
			}
			return os.WriteFile(dst, data, 0600)
		})
		if e != nil {
			t.Fatal(e)
		}
	}
	cleanServers(context.Background())
	e := newApp()
	owner := webhookTestServer(t, e)
	_, other := registerAndLogin(t, e)
	channel := createChannelFor(t, "plugin_"+randHex(4))
	endpoints := map[string]string{"/plugins": "GET", "/plugins/rescan": "POST", "/plugins/reference/preview": "POST", "/plugins/reference/enable": "POST", "/plugins/reference/disable": "POST", "/plugins/reference/trigger": "POST", "/plugins/reference": "PATCH"}
	for endpoint, method := range endpoints {
		for _, cookie := range []string{"", other} {
			want := 403
			if cookie == "" {
				want = 401
			}
			if rec := do(t, e, method, endpoint, []byte(`{}`), authCookie(cookie)); rec.Code != want {
				t.Fatalf("auth %s: %d %s", endpoint, rec.Code, rec.Body.String())
			}
		}
	}
	if rec := do(t, e, "POST", "/plugins/rescan", []byte(`{}`), authCookie(owner)); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var installed []models.AppInstallation
	for _, id := range []string{"bluemap", "reference"} {
		settings := map[string]any{"map_url": "https://8.8.8.8/map"}
		if id == "reference" {
			settings = map[string]any{"target_channel": channel.ID}
		}
		opts := services.PluginOptions{Settings: settings}
		body, _ := json.Marshal(opts)
		rec := do(t, e, "POST", "/plugins/"+id+"/preview", body, authCookie(owner))
		if rec.Code != 200 {
			t.Fatalf("preview %d %s", rec.Code, rec.Body.String())
		}
		var preview services.PluginPreview
		if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
			t.Fatal(err)
		}
		opts.Consent = preview.Consent
		opts.ExpiresAt = preview.ExpiresAt
		body, _ = json.Marshal(opts)
		rec = do(t, e, "POST", "/plugins/"+id+"/enable", body, authCookie(owner))
		if rec.Code != 200 {
			t.Fatalf("enable %d %s", rec.Code, rec.Body.String())
		}
		var a models.AppInstallation
		if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
			t.Fatal(err)
		}
		installed = append(installed, a)
		// APIs antigas não podem ampliar grants/escopos por fora do consentimento.
		if rec := do(t, e, "PATCH", "/apps/"+a.ID, []byte(`{"capabilities":{"read_messages":true}}`), authCookie(owner)); rec.Code != 403 {
			t.Fatal("bypass do consentimento", rec.Code)
		}
		if rec := do(t, e, "PUT", "/apps/"+a.ID+"/channels/"+channel.ID, nil, authCookie(owner)); rec.Code != 403 {
			t.Fatal("bypass de escopo", rec.Code)
		}
	}
	var count int
	if err := storage.GetDB().QueryRow("SELECT count(*) FROM app_channels WHERE app_installation_id IN ($1,$2)", installed[0].ID, installed[1].ID).Scan(&count); err != nil || count != 2 {
		t.Fatal("canais app ausentes", count, err)
	}
	rec := do(t, e, "POST", "/plugins/reference/trigger", []byte(`{}`), authCookie(owner))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"source_type":"app"`) {
		t.Fatalf("trigger %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, e, "GET", "/apps/"+installed[1].ID+"/ui/index.html", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("UI %d %s", rec.Code, rec.Body.String())
	}
	for name, want := range map[string]string{"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "Cross-Origin-Resource-Policy": "cross-origin", "Access-Control-Allow-Origin": "*"} {
		if rec.Header().Get(name) != want {
			t.Fatal(name, rec.Header())
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox allow-scripts allow-forms") || strings.Contains(csp, "allow-same-origin") {
		t.Fatal(csp)
	}
	for _, id := range []string{"reference", "bluemap"} {
		rec = do(t, e, "POST", "/plugins/"+id+"/disable", nil, authCookie(owner))
		if rec.Code != 204 {
			t.Fatal(rec.Body.String())
		}
	}
	if err := storage.GetDB().QueryRow("SELECT count(*) FROM app_channels WHERE app_installation_id IN ($1,$2)", installed[0].ID, installed[1].ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("canais sobrevivem disable", count, err)
	}
	rec = do(t, e, "GET", "/apps/"+installed[1].ID+"/ui/index.html", nil, nil)
	if rec.Code != 404 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("UI desabilitada disponível", rec.Code)
	}
	if rec := do(t, e, "PUT", "/apps/"+installed[1].ID+"/artifact", []byte("wasm"), authCookie(owner)); rec.Code != 404 && rec.Code != 405 {
		t.Fatal("upload legado disponível", rec.Code)
	}
	if rec := do(t, e, http.MethodPost, "/apps", []byte(`{"app_key":"bypass","kind":"plugin","name":"bypass"}`), authCookie(owner)); rec.Code != 400 {
		t.Fatal("plugin criado fora do lifecycle", rec.Code)
	}
}
