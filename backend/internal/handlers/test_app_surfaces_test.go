package handlers

import (
	"context"
	"encoding/json"
	ws "github.com/gorilla/websocket"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/storage"
	"papo/internal/websocket"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// handlerSurfaceSource fornece o contrato da futura descoberta de plugins aos testes HTTP.
type handlerSurfaceSource map[string]services.PluginSurfaceBundle

// Resolve retorna metadados sem instanciar um módulo Wasm.
func (s handlerSurfaceSource) Resolve(_ context.Context, id string) (services.PluginSurfaceBundle, error) {
	b, ok := s[id]
	if !ok {
		return b, services.ErrAppNotFound
	}
	return b, nil
}

// newHandlerSurfaces cria uma instalação e assets estáticos de teste, sem token.
func newHandlerSurfaces(t *testing.T, ownerID string) (models.AppInstallation, handlerSurfaceSource) {
	t.Helper()
	a, _, err := services.CreateApp(context.Background(), ownerID, "fixture", "plugin", "App", models.AppCapabilities{SendMessages: true, PostAppEmbeds: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "ui"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"index.html", "result.html", "style.css", "main.mjs", "image.svg", "fixture.ttf", "data.json"} {
		data, err := os.ReadFile(filepath.Join("../../../examples/plugins/reference/ui", file))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ui", file), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for file, data := range map[string]string{"plugin.wasm": "\x00asm\x01\x00\x00\x00", "secret.txt": "segredo fora de ui"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}

	source := handlerSurfaceSource{a.ID: {Directory: dir, Digest: "digest-v1", Enabled: true, Surfaces: []services.AppSurfaceDeclaration{
		{ID: "map", Kind: "channel", DefaultName: "app_" + randHex(4), Source: "bundled", Entry: "ui/index.html"},
		{ID: "result", Kind: "embed", Source: "bundled", Entry: "ui/result.html", FrameWidth: 480, FrameHeight: 320},
	}}}
	services.SetPluginSurfaceSource(source)
	t.Cleanup(func() { services.SetPluginSurfaceSource(nil) })
	return a, source
}

// TestAppUIStaticRoute cobre isolamento, cookies ignorados, cache, desativação e confinamento.
func TestAppUIStaticRoute(t *testing.T) {
	t.Setenv("BASE_URL", "https://papo.com:8443/proxy/")
	t.Setenv("APPS_ORIGIN", "")
	cleanServers(context.Background())
	e := newApp()
	id, _ := registerAndLogin(t, e)
	a, source := newHandlerSurfaces(t, id)
	e.Use(middleware.CORS([]string{"*"}))
	path := "/apps/" + a.ID + "/ui/"
	rec := do(t, e, http.MethodGet, path+"index.html", nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Fatal(rec.Code, rec.Body.String())
	}
	for name, want := range map[string]string{"Content-Security-Policy": "sandbox allow-scripts allow-forms; default-src 'self' https://papo.com:8443", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "Cross-Origin-Resource-Policy": "cross-origin", "Access-Control-Allow-Origin": "*", "Cache-Control": "public, max-age=0, must-revalidate"} {
		if got := rec.Header().Get(name); got != want {
			t.Fatalf("%s: %q", name, got)
		}
	}
	if rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("ETag") == "" {
		t.Fatal(rec.Header())
	}
	for _, origin := range []string{"null", "https://client.example"} {
		for _, asset := range []string{"index.html", "style.css", "main.mjs", "image.svg", "fixture.ttf", "data.json"} {
			req := httptest.NewRequest(http.MethodGet, path+asset, nil)
			req.Header.Set("Origin", origin)
			out := httptest.NewRecorder()
			e.ServeHTTP(out, req)
			if out.Code != 200 || out.Header().Get("Access-Control-Allow-Origin") != "*" || out.Header().Get("Access-Control-Allow-Credentials") != "" || out.Header().Get("Cross-Origin-Resource-Policy") != "cross-origin" {
				t.Fatalf("asset %s origin %s: %d %v %s", asset, origin, out.Code, out.Header(), out.Body.String())
			}
		}
	}
	tag := rec.Header().Get("ETag")
	rec = do(t, e, http.MethodGet, path+"index.html", nil, authCookie("sessao-invalida"))
	if rec.Code != 200 || rec.Header().Get("Set-Cookie") != "" {
		t.Fatal(rec.Code, rec.Header())
	}
	req := httptest.NewRequest(http.MethodGet, path+"index.html", nil)
	req.Header.Set("If-None-Match", tag)
	cached := httptest.NewRecorder()
	e.ServeHTTP(cached, req)
	if cached.Code != 304 {
		t.Fatal(cached.Code, cached.Body.String())
	}
	b := source[a.ID]
	b.Digest = "digest-v2"
	source[a.ID] = b
	req = httptest.NewRequest(http.MethodGet, path+"index.html", nil)
	req.Header.Set("If-None-Match", tag)
	changed := httptest.NewRecorder()
	e.ServeHTTP(changed, req)
	if changed.Code != 200 || changed.Header().Get("ETag") == tag {
		t.Fatal(changed.Code, changed.Header())
	}
	for _, target := range []string{"", "missing.html", "../plugin.wasm", "%2e%2e/secret.txt", "%252e%252e/secret.txt", "..%5csecret.txt", "plugin.toml", "/etc/passwd"} {
		rec := do(t, e, http.MethodGet, path+target, nil, nil)
		if rec.Code != 404 {
			t.Fatalf("escape %s: %d %s", target, rec.Code, rec.Body.String())
		}
	}
	if err := os.Symlink(filepath.Join(b.Directory, "secret.txt"), filepath.Join(b.Directory, "ui", "outside.html")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("index.html", filepath.Join(b.Directory, "ui", "inside.html")); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"outside.html", "inside.html"} {
		if rec := do(t, e, http.MethodGet, path+target, nil, nil); rec.Code != 404 {
			t.Fatal("symlink", rec.Code)
		}
	}
	b.ApprovedOrigins = []string{"https://8.8.8.8"}
	source[a.ID] = b
	rec = do(t, e, http.MethodGet, path+"index.html", nil, nil)
	if rec.Code != 200 || rec.Header().Get("Content-Security-Policy") != "sandbox allow-scripts allow-forms; default-src 'self' https://papo.com:8443 https://8.8.8.8" {
		t.Fatal(rec.Code, rec.Header())
	}
	b.Enabled = false
	source[a.ID] = b
	rec = do(t, e, http.MethodGet, path+"index.html", nil, nil)
	if rec.Code != 404 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(rec.Code, rec.Header())
	}
	b.Enabled = true
	source[a.ID] = b
	if err := services.RevokeApp(context.Background(), id, a.ID); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, e, http.MethodGet, path+"index.html", nil, nil); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
}

// TestAppUICORSNullOrigin garante a mesma política da API mesmo com wildcard e origem explícita.
func TestAppUICORSNullOrigin(t *testing.T) {
	for _, origins := range [][]string{{"*"}, {"null"}, {"https://example.org"}} {
		e := newApp()
		e.Use(middleware.CORS(origins))
		for _, method := range []string{http.MethodGet, http.MethodOptions} {
			req := httptest.NewRequest(method, "/auth/whoami", nil)
			req.Header.Set("Origin", "null")
			req.Header.Set("Access-Control-Request-Method", "GET")
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal(origins, rec.Header())
			}
		}
	}
}

// readAppChannelEvent ignora presença, mas valida tipo e canal do evento aguardado.
func readAppChannelEvent(t *testing.T, conn *ws.Conn, kind websocket.EventType, id string) map[string]json.RawMessage {
	t.Helper()
	for {
		data := readWSMessage(t, conn)
		var event map[string]json.RawMessage
		if err := json.Unmarshal(data, &event); err != nil {
			t.Fatal(err)
		}
		var typ websocket.EventType
		json.Unmarshal(event["type"], &typ)
		if typ == websocket.EventTypePresenceUpdate || typ == websocket.EventTypePresenceSync {
			continue
		}
		var channel string
		json.Unmarshal(event["channel_id"], &channel)
		if typ != kind || channel != id {
			t.Fatalf("evento inesperado: %s", data)
		}
		if _, ok := event["views"]; ok {
			t.Fatal("modelo removido", string(data))
		}
		return event
	}
}

// assertNoAppChannelEvent exige ausência de eventos de canal para o usuário sem leitura.
func assertNoAppChannelEvent(t *testing.T, conn *ws.Conn) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(120 * time.Millisecond))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if n, ok := err.(net.Error); ok && n.Timeout() {
				return
			}
			t.Fatal(err)
		}
		var event struct {
			Type websocket.EventType `json:"type"`
		}
		json.Unmarshal(data, &event)
		if event.Type == websocket.EventTypeChannelCreate || event.Type == websocket.EventTypeChannelUpdate || event.Type == websocket.EventTypeChannelDelete || event.Type == websocket.EventTypeAppInvalidate {
			t.Fatal("canal privado vazou", string(data))
		}
	}
}

// TestAppChannelEvents cobre os hooks e as rotas de gestão com eventos restritos aos leitores.
func TestAppChannelEvents(t *testing.T) {
	go websocket.GetHub().Run()
	for _, trigger := range []string{"create", "reconcile", "rename", "move", "permissions", "delete_hook", "revoke", "invalidate"} {
		t.Run(trigger, func(t *testing.T) {
			cleanServers(context.Background())
			e := newApp()
			ownerID, owner := registerAndLogin(t, e)
			createServerFor(t, ownerID)
			readerID, reader := registerAndLogin(t, e)
			_, outsider := registerAndLogin(t, e)
			managerID, manager := registerAndLogin(t, e)
			manage := createRoleFor(t, models.RolePermissions{ManageApps: true, ManageChannels: true})
			assignRoleToUser(t, managerID, manage.ID)
			a, source := newHandlerSurfaces(t, ownerID)
			b := source[a.ID]
			changes, err := services.CreateAppChannels(context.Background(), ownerID, a.ID, b.Surfaces)
			if err != nil {
				t.Fatal(err)
			}
			ch := *changes[0].After
			role := createRoleFor(t, models.RolePermissions{})
			assignRoleToUser(t, readerID, role.ID)
			if trigger != "create" {
				if _, err := storage.UpdateChannelPermissions(context.Background(), ch.ID, role.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
					t.Fatal(err)
				}
			}
			srv := httptest.NewServer(e)
			defer srv.Close()
			dial := func(token string) *ws.Conn {
				t.Helper()
				header := http.Header{}
				header.Set("Cookie", "Auth="+token)
				conn, _, err := ws.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", header)
				if err != nil {
					t.Fatal(err)
				}
				readWSMessage(t, conn)
				return conn
			}
			outsiderConn := dial(outsider)
			defer outsiderConn.Close()
			readerConn := dial(reader)
			defer readerConn.Close()
			ownerConn := dial(owner)
			defer ownerConn.Close()
			kind := websocket.EventTypeChannelUpdate
			switch trigger {
			case "invalidate":
				BroadcastAppInvalidation(context.Background(), a.ID)
				kind = websocket.EventTypeAppInvalidate
			case "create":
				BroadcastAppChannelChanges(context.Background(), changes)
				kind = websocket.EventTypeChannelCreate
			case "reconcile":
				b.Surfaces[0].Entry = "ui/result.html"
				source[a.ID] = b
				changes, err = services.ReconcileAppChannels(context.Background(), ownerID, a.ID, b.Surfaces)
				if err != nil {
					t.Fatal(err)
				}
				BroadcastAppChannelChanges(context.Background(), changes)
			case "rename":
				if rec := do(t, e, http.MethodPut, "/channels/"+ch.ID, []byte(`{"name":"renamed"}`), authCookie(manager)); rec.Code != 200 {
					t.Fatal(rec.Code, rec.Body.String())
				}
			case "move":
				body, _ := json.Marshal(map[string]int{"old_position": ch.Position, "new_position": 1})
				if rec := do(t, e, http.MethodPut, "/channels/"+ch.ID+"/change_position", body, authCookie(manager)); rec.Code != 200 {
					t.Fatal(rec.Code, rec.Body.String())
				}
			case "permissions":
				body := []byte(`{"permissions":{"read_channel":true}}`)
				if rec := do(t, e, http.MethodPut, "/channels/"+ch.ID+"/permissions/"+role.ID, body, authCookie(manager)); rec.Code != 200 {
					t.Fatal(rec.Code, rec.Body.String())
				}
			case "delete_hook":
				changes, err = services.DeleteAppChannels(context.Background(), ownerID, a.ID)
				if err != nil {
					t.Fatal(err)
				}
				BroadcastAppChannelChanges(context.Background(), changes)
				kind = websocket.EventTypeChannelDelete
			case "revoke":
				// O endpoint legado agora rejeita plugins; o hook continua preservando snapshots de revoke.
				if rec := do(t, e, http.MethodDelete, "/apps/"+a.ID, nil, authCookie(manager)); rec.Code != 403 {
					t.Fatal(rec.Code)
				}
				changes, err = services.RevokeAppWithChannelChanges(context.Background(), ownerID, a.ID)
				if err != nil {
					t.Fatal(err)
				}
				BroadcastAppChannelChanges(context.Background(), changes)
				kind = websocket.EventTypeChannelDelete
			}
			for _, conn := range []*ws.Conn{readerConn, ownerConn} {
				if kind == websocket.EventTypeAppInvalidate {
					for {
						event := readWSMessage(t, conn)
						var payload struct {
							Type websocket.EventType `json:"type"`
							ID   string              `json:"app_installation_id"`
						}
						if err := json.Unmarshal(event, &payload); err != nil {
							t.Fatal(err)
						}
						if payload.Type == websocket.EventTypePresenceUpdate || payload.Type == websocket.EventTypePresenceSync {
							continue
						}
						if payload.Type != websocket.EventTypeAppInvalidate || payload.ID != a.ID {
							t.Fatalf("invalidation: %s", event)
						}
						break
					}
					continue
				}
				event := readAppChannelEvent(t, conn, kind, ch.ID)
				if kind != websocket.EventTypeChannelDelete {
					var app models.AppChannel
					if err := json.Unmarshal(event["app"], &app); err != nil || !app.Available || !strings.HasPrefix(app.Entry, "/apps/"+a.ID+"/ui/") {
						t.Fatal(app, err)
					}
				}
			}
			if trigger == "create" {
				readAppChannelEvent(t, outsiderConn, kind, ch.ID)
			} else {
				assertNoAppChannelEvent(t, outsiderConn)
			}
		})
	}
}

// TestAppChannelManualActions bloqueia CRUD antigo, exclusão manual e mudanças de tipo.
func TestAppChannelManualActions(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	id, owner := registerAndLogin(t, e)
	createServerFor(t, id)
	a, source := newHandlerSurfaces(t, id)
	changes, err := services.CreateAppChannels(context.Background(), id, a.ID, source[a.ID].Surfaces)
	if err != nil {
		t.Fatal(err)
	}
	ch := changes[0].After
	if rec := do(t, e, http.MethodPut, "/channels/"+ch.ID, []byte(`{"name":"app-same-type","type":"app"}`), authCookie(owner)); rec.Code != 200 {
		t.Fatal("renomear preservando tipo", rec.Code, rec.Body.String())
	}
	for _, request := range []struct{ method, path, body string }{{http.MethodDelete, "/channels/" + ch.ID, ""}, {http.MethodPut, "/channels/" + ch.ID, `{"name":"app","type":"text"}`}} {
		rec := do(t, e, request.method, request.path, []byte(request.body), authCookie(owner))
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "desative o app") {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	if rec := do(t, e, http.MethodPost, "/channels/"+ch.ID+"/views", []byte(`{}`), authCookie(owner)); rec.Code != 404 {
		t.Fatal("rota antiga", rec.Code)
	}
	_, outsider := registerAndLogin(t, e)
	role := createRoleFor(t, models.RolePermissions{})
	if _, err := storage.UpdateChannelPermissions(context.Background(), ch.ID, role.ID, models.ChannelPermission{}); err != nil {
		t.Fatal(err)
	}
	rec := do(t, e, http.MethodGet, "/channels", nil, authCookie(outsider))
	var list channelListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, channel := range list.Channels {
		if channel.ID == ch.ID && channel.App != nil {
			t.Fatal("superfície privada", channel)
		}
	}
}

// TestAppChannelTypingRejected impede typing no app sem retirar a leitura da superfície.
func TestAppChannelTypingRejected(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	id, token := registerAndLogin(t, e)
	createServerFor(t, id)
	a, source := newHandlerSurfaces(t, id)
	changes, err := services.CreateAppChannels(context.Background(), id, a.ID, source[a.ID].Surfaces)
	if err != nil {
		t.Fatal(err)
	}
	go websocket.GetHub().Run()
	srv := httptest.NewServer(e)
	defer srv.Close()
	header := http.Header{}
	header.Set("Cookie", "Auth="+token)
	conn, _, err := ws.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	readWSMessage(t, conn)
	if err := conn.WriteJSON(map[string]string{"type": "typing", "channel_id": changes[0].After.ID}); err != nil {
		t.Fatal(err)
	}
	var event struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(readWSMessage(t, conn), &event); err != nil || event.Type != "error" {
		t.Fatal(event, err)
	}
}

// TestAppUIOpaqueOriginAssets cobre recursos reais, CORS global e origens vindas da configuração.
func TestAppUIOpaqueOriginAssets(t *testing.T) {
	t.Setenv("BASE_URL", "https://papo.example:8443/api/")
	t.Setenv("APPS_ORIGIN", "")
	cleanServers(context.Background())
	id, _ := registerAndLogin(t, newApp())
	a, source := newHandlerSurfaces(t, id)
	b := source[a.ID]
	b.ApprovedOrigins = []string{"https://8.8.8.8"}
	source[a.ID] = b
	for _, served := range []struct{ name, apps, origin string }{
		{"base_url", "", "https://papo.example:8443"},
		{"apps_origin", "https://apps.example:9443/", "https://apps.example:9443"},
	} {
		t.Run(served.name, func(t *testing.T) {
			t.Setenv("APPS_ORIGIN", served.apps)
			e := newApp()
			services.SetPluginSurfaceSource(source)
			e.Use(middleware.CORS([]string{"https://client.example"}))
			for _, asset := range []struct{ file, mime string }{
				{"index.html", "text/html"}, {"result.html", "text/html"}, {"style.css", "text/css"}, {"main.mjs", "javascript"},
				{"image.svg", "image/svg+xml"}, {"fixture.ttf", "font/ttf"}, {"data.json", "application/json"},
			} {
				for _, origin := range []string{"null", "https://client.example"} {
					target := "/apps/" + a.ID + "/ui/" + asset.file
					req := httptest.NewRequest(http.MethodGet, target, nil)
					req.Header.Set("Origin", origin)
					req.Host = "untrusted-host.example"
					req.Header.Set("X-Forwarded-Host", "untrusted-forwarded.example")
					rec := httptest.NewRecorder()
					e.ServeHTTP(rec, req)
					if rec.Code != 200 {
						t.Fatalf("%s: %d %s", asset.file, rec.Code, rec.Body.String())
					}
					headers := rec.Header()
					want := "sandbox allow-scripts allow-forms; default-src 'self' " + served.origin + " https://8.8.8.8"
					if headers.Get("Content-Security-Policy") != want || headers.Get("Cross-Origin-Resource-Policy") != "cross-origin" || headers.Get("Access-Control-Allow-Origin") != "*" || headers.Get("Access-Control-Allow-Credentials") != "" || headers.Get("Set-Cookie") != "" {
						t.Fatalf("%s Origin=%s: %v", asset.file, origin, headers)
					}
					if !strings.Contains(headers.Get("Content-Type"), asset.mime) || rec.Body.Len() == 0 {
						t.Fatal(asset.file, headers.Get("Content-Type"))
					}
					// A mesma política é necessária na revalidação, não somente na primeira carga.
					req = httptest.NewRequest(http.MethodGet, target, nil)
					req.Header.Set("Origin", origin)
					req.Header.Set("If-None-Match", headers.Get("ETag"))
					cached := httptest.NewRecorder()
					e.ServeHTTP(cached, req)
					if cached.Code != 304 || cached.Header().Get("Access-Control-Allow-Origin") != "*" || cached.Header().Get("Access-Control-Allow-Credentials") != "" || cached.Header().Get("Cross-Origin-Resource-Policy") != "cross-origin" || cached.Header().Get("Content-Security-Policy") != want {
						t.Fatal(asset.file, cached.Code, cached.Header())
					}
				}
			}
		})
	}
	t.Setenv("APPS_ORIGIN", "https://apps.example/; script-src *")
	e := newApp()
	services.SetPluginSurfaceSource(source)
	if rec := do(t, e, http.MethodGet, "/apps/"+a.ID+"/ui/index.html", nil, nil); rec.Code != 404 {
		t.Fatal("configuração inválida", rec.Code)
	}
}
