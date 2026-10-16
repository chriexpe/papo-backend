package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/storage"
	"papo/internal/utils"
	"papo/internal/websocket"
)

// createAppViaRoute instala um app e verifica que o segredo só existe no modo remoto.
func createAppViaRoute(t *testing.T, e *echo.Echo, cookie, kind string) (models.AppInstallation, string) {
	t.Helper()
	if kind == "plugin" {
		id, e := utils.ValidateToken(cookie, config.LoadConfig().JWTSecret)
		if e != nil {
			t.Fatal(e)
		}
		a, token, e := services.CreateApp(context.Background(), id, "fixture-"+randHex(4), kind, "Bot", models.AppCapabilities{SendMessages: true, ReadMessages: true})
		if e != nil {
			t.Fatal(e)
		}
		return a, token
	}
	body, _ := json.Marshal(map[string]any{"app_key": "teste", "kind": kind, "name": "Bot", "capabilities": models.AppCapabilities{SendMessages: true, ReadMessages: true}})
	rec := do(t, e, http.MethodPost, "/apps", body, authCookie(cookie))
	if rec.Code != 201 {
		t.Fatalf("criação: %d %s", rec.Code, rec.Body.String())
	}
	var response createAppResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if (kind == "remote") != (response.Token != "") {
		t.Fatal("token incompatível com tipo")
	}
	return response.AppInstallation, response.Token
}

// scopeAppViaRoute concede acesso ao canal e exige sucesso.
func scopeAppViaRoute(t *testing.T, e *echo.Echo, cookie, id, channel string) {
	t.Helper()
	rec := do(t, e, http.MethodPut, "/apps/"+id+"/channels/"+channel, nil, authCookie(cookie))
	if rec.Code != 204 {
		t.Fatalf("escopo: %d %s", rec.Code, rec.Body.String())
	}
}

// TestAppManagementRequiresManageApps garante independência de manage_channels e manage_webhooks.
func TestAppManagementRequiresManageApps(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	owner := webhookTestServer(t, e)
	id, token := registerAndLogin(t, e)
	body := []byte(`{"app_key":"test","kind":"remote","name":"Bot"}`)
	for _, perms := range []models.RolePermissions{{}, {ManageChannels: true}, {ManageWebhooks: true}} {
		role := createRoleFor(t, perms)
		assignRoleToUser(t, id, role.ID)
		assertProblem(t, do(t, e, http.MethodPost, "/apps", body, authCookie(token)), 403, "forbidden", "Acesso negado", "usuário não possui a permissão necessária para esta operação")
	}
	role := createRoleFor(t, models.RolePermissions{ManageApps: true})
	assignRoleToUser(t, id, role.ID)
	createAppViaRoute(t, e, token, "remote")
	createAppViaRoute(t, e, owner, "remote")
	// A direção inversa também é independente.
	other, otherToken := registerAndLogin(t, e)
	assignRoleToUser(t, other, role.ID)
	channel := createChannelFor(t, "independente_"+randHex(4))
	payload, _ := json.Marshal(map[string]string{"channel_id": channel.ID, "name": "hook"})
	assertProblem(t, do(t, e, http.MethodPost, "/webhooks", payload, authCookie(otherToken)), 403, "forbidden", "Acesso negado", "usuário não possui a permissão necessária para esta operação")
}

// TestAppAuthenticationRoutes cobre a matriz de autenticação em todas as operações remotas.
func TestAppAuthenticationRoutes(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	owner := webhookTestServer(t, e)
	channel := createChannelFor(t, "app_auth_"+randHex(4))
	a, token := createAppViaRoute(t, e, owner, "remote")
	scopeAppViaRoute(t, e, owner, a.ID, channel.ID)
	path := "/app/channels/" + channel.ID + "/messages"
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodPut, http.MethodDelete} {
		target := path
		if method == http.MethodPut || method == http.MethodDelete {
			target += "/00000000-0000-0000-0000-000000000000"
		}
		for _, bad := range []string{"", "errado", owner} {
			rec := doWithBearer(t, e, method, target, nil, bad)
			assertProblem(t, rec, 401, "unauthorized", "Não autorizado", "app inválido")
		}
		rec := do(t, e, method, target, nil, authCookie(owner))
		assertProblem(t, rec, 401, "unauthorized", "Não autorizado", "app inválido")
	}
	for _, kind := range []string{"plugin"} {
		local, secret := createAppViaRoute(t, e, owner, kind)
		if secret != "" {
			t.Fatal("segredo local")
		}
		var hash *string
		if err := storage.GetDB().QueryRow("SELECT secret_hash FROM app_installations WHERE id=$1", local.ID).Scan(&hash); err != nil || hash != nil {
			t.Fatalf("hash local: %v %v", hash, err)
		}
		rec := do(t, e, http.MethodPost, "/apps/"+local.ID+"/token", nil, authCookie(owner))
		if rec.Code != 400 {
			t.Fatal(rec.Body.String())
		}
	}
	rec := do(t, e, http.MethodPost, "/apps/"+a.ID+"/token", nil, authCookie(owner))
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var rotated map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	assertProblem(t, doWithBearer(t, e, http.MethodGet, path, nil, token), 401, "unauthorized", "Não autorizado", "app inválido")
	if rec = doWithBearer(t, e, http.MethodGet, path, nil, rotated["token"]); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if rec = do(t, e, http.MethodDelete, "/apps/"+a.ID, nil, authCookie(owner)); rec.Code != 204 {
		t.Fatal(rec.Body.String())
	}
	assertProblem(t, doWithBearer(t, e, http.MethodGet, path, nil, rotated["token"]), 401, "unauthorized", "Não autorizado", "app inválido")
}

// TestAppMessageRoutes cobre upload acima de 4MB, retry completo, edição, histórico e exclusão.
func TestAppMessageRoutes(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	owner := webhookTestServer(t, e)
	channel := createChannelFor(t, "app_messages_"+randHex(4))
	a, token := createAppViaRoute(t, e, owner, "remote")
	scopeAppViaRoute(t, e, owner, a.ID, channel.ID)
	path := "/app/channels/" + channel.ID + "/messages"
	fields := map[string]string{"content": "original", "external_message_id": "retry", "embeds": "[{\"title\":\"Embed\"}]"}
	first := doMultipartWithBearer(t, e, http.MethodPost, path, fields, map[string][][2]string{"attachments": {{"big.bin", string(make([]byte, 5<<20))}}}, token)
	if first.Code != 201 {
		t.Fatalf("envio: %d %s", first.Code, first.Body.String())
	}
	var m models.MessageWithAttachment
	if err := json.Unmarshal(first.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	retry := doMultipartWithBearer(t, e, http.MethodPost, path, map[string]string{"content": "ignorado", "external_message_id": "retry"}, nil, token)
	var retried models.MessageWithAttachment
	if retry.Code != 200 || json.Unmarshal(retry.Body.Bytes(), &retried) != nil || retried.ID != m.ID || len(retried.Attachments) != 1 || len(retried.Embeds) != 1 {
		t.Fatalf("retry: %d %s", retry.Code, retry.Body.String())
	}
	if m.AppInstallationID == nil || *m.AppInstallationID != a.ID || m.ExternalAuthor == nil || m.ExternalAuthor.Source != "app" || m.AuthorID != nil {
		t.Fatalf("atribuição: %+v", m)
	}
	// O contrato WS preserva author_id vazio e expõe app_installation_id.
	event, _ := json.Marshal(websocket.MessageOutbound{AuthorID: derefString(m.AuthorID), AppInstallationID: m.AppInstallationID, ExternalAuthor: m.ExternalAuthor})
	var outbound map[string]any
	json.Unmarshal(event, &outbound)
	if outbound["author_id"] != "" || outbound["app_installation_id"] != a.ID {
		t.Fatalf("WS: %s", event)
	}
	if rec := doWithBearer(t, e, http.MethodPut, path+"/"+m.ID, []byte(`{"content":"editada"}`), token); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	history := doWithBearer(t, e, http.MethodGet, path+"?limit=1&order=asc", nil, token)
	var page models.MessageList
	if history.Code != 200 || json.Unmarshal(history.Body.Bytes(), &page) != nil || len(page.Messages) != 1 || page.Messages[0].Content == nil || *page.Messages[0].Content != "editada" || len(page.Messages[0].Embeds) != 1 {
		t.Fatalf("histórico: %s", history.Body.String())
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=x", "since=inválido", "order=inválido", "since=2026-01-01T00:00:00Z&last_id=inválido"} {
		rec := doWithBearer(t, e, http.MethodGet, path+"?"+query, nil, token)
		if rec.Code != 400 {
			t.Fatalf("query %s: %d %s", query, rec.Code, rec.Body.String())
		}
	}
	if rec := doWithBearer(t, e, http.MethodDelete, path+"/"+m.ID, nil, token); rec.Code != 204 {
		t.Fatal(rec.Body.String())
	}
	// A deduplicação acaba quando a mensagem é removida.
	if rec := doMultipartWithBearer(t, e, http.MethodPost, path, fields, nil, token); rec.Code != 201 {
		t.Fatal(rec.Body.String())
	}
}

// TestAppScopeRoutes cobre canais privados, DMs e remoção de capacidade/escopo em uso.
func TestAppScopeRoutes(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	owner := webhookTestServer(t, e)
	actor, actorToken := registerAndLogin(t, e)
	role := createRoleFor(t, models.RolePermissions{ManageApps: true})
	assignRoleToUser(t, actor, role.ID)
	a, token := createAppViaRoute(t, e, actorToken, "remote")
	private := createChannelFor(t, "privado_"+randHex(4))
	if _, err := storage.UpdateChannelPermissions(context.Background(), private.ID, role.ID, models.ChannelPermission{}); err != nil {
		t.Fatal(err)
	}
	assertProblem(t, do(t, e, http.MethodPut, "/apps/"+a.ID+"/channels/"+private.ID, nil, authCookie(actorToken)), 403, "forbidden", "Acesso negado", "operação não permitida")
	dm, err := storage.CreateChannel(context.Background(), "dm_"+randHex(4), "dm", "")
	if err != nil {
		t.Fatal(err)
	}
	assertProblem(t, do(t, e, http.MethodPut, "/apps/"+a.ID+"/channels/"+dm.ID, nil, authCookie(owner)), 403, "forbidden", "Acesso negado", "operação não permitida")
	for _, ch := range []models.Channel{private, dm, createChannelFor(t, "sem_escopo_"+randHex(4))} {
		path := "/app/channels/" + ch.ID + "/messages"
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
			target := path
			if method == http.MethodPut || method == http.MethodDelete {
				target += "/00000000-0000-0000-0000-000000000000"
			}
			var rec *httptest.ResponseRecorder
			if method == http.MethodPost {
				rec = doMultipartWithBearer(t, e, method, target, map[string]string{"content": "x"}, nil, token)
			} else {
				rec = doWithBearer(t, e, method, target, []byte(`{"content":"x"}`), token)
			}
			assertProblem(t, rec, 403, "forbidden", "Acesso negado", "operação não permitida")
		}
	}
	// Escopo explícito do dono permite canal privado: a ACL de máquina é o escopo.
	scopeAppViaRoute(t, e, owner, a.ID, private.ID)
	path := "/app/channels/" + private.ID + "/messages"
	if rec := doWithBearer(t, e, http.MethodGet, path, nil, token); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	rec := do(t, e, http.MethodPatch, "/apps/"+a.ID, []byte(`{"capabilities":{"read_messages":false,"send_messages":false}}`), authCookie(actorToken))
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	assertProblem(t, doWithBearer(t, e, http.MethodGet, path, nil, token), 403, "forbidden", "Acesso negado", "operação não permitida")
	rec = do(t, e, http.MethodDelete, "/apps/"+a.ID+"/channels/"+private.ID, nil, authCookie(actorToken))
	if rec.Code != 204 {
		t.Fatal(rec.Body.String())
	}
}

// TestAppIdentityAndListRoutes cobre avatar público, PATCH, paginação e ausência de segredo nas leituras.
func TestAppIdentityAndListRoutes(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	owner := webhookTestServer(t, e)
	a, token := createAppViaRoute(t, e, owner, "remote")
	if rec := do(t, e, http.MethodGet, "/apps/"+a.ID+"/avatar", nil, nil); rec.Code != 404 {
		t.Fatal(rec.Body.String())
	}
	avatar := base64.StdEncoding.EncodeToString(pngAvatarBytes(32, 32))
	patch, _ := json.Marshal(map[string]string{"name": "Renomeado", "display_name": "Nick", "avatar": avatar, "avatar_format": "png"})
	if rec := do(t, e, http.MethodPatch, "/apps/"+a.ID, patch, authCookie(owner)); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if rec := do(t, e, http.MethodGet, "/apps/"+a.ID+"/avatar", nil, nil); rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatal(rec.Body.String())
	}
	page := do(t, e, http.MethodGet, "/apps?limit=1", nil, authCookie(owner))
	var list models.AppList
	if page.Code != 200 || json.Unmarshal(page.Body.Bytes(), &list) != nil || len(list.Apps) != 1 {
		t.Fatalf("lista: %s", page.Body.String())
	}
	var raw map[string]any
	json.Unmarshal(page.Body.Bytes(), &raw)
	item := raw["apps"].([]any)[0].(map[string]any)
	if item["token"] != nil || item["secret_hash"] != nil {
		t.Fatal("segredo exposto")
	}
	ch := createChannelFor(t, "identity_"+randHex(4))
	scopeAppViaRoute(t, e, owner, a.ID, ch.ID)
	rec := doMultipartWithBearer(t, e, http.MethodPost, "/app/channels/"+ch.ID+"/messages", map[string]string{"content": "x"}, nil, token)
	var message models.MessageWithAttachment
	if rec.Code != 201 || json.Unmarshal(rec.Body.Bytes(), &message) != nil || message.ExternalAuthor.Name != "Nick" || message.ExternalAuthor.AvatarURL != "/apps/"+a.ID+"/avatar" {
		t.Fatal(rec.Body.String())
	}
	if rec = doMultipartWithBearer(t, e, http.MethodPost, "/app/channels/"+ch.ID+"/messages", map[string]string{"content": "x", "external_author": "{}"}, nil, token); rec.Code != 400 {
		t.Fatal(rec.Body.String())
	}
}

// TestAppRateLimitIsPerInstallation garante que um app não consome o burst de outro.
func TestAppRateLimitIsPerInstallation(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	owner := webhookTestServer(t, e)
	a, token := createAppViaRoute(t, e, owner, "remote")
	b, other := createAppViaRoute(t, e, owner, "remote")
	ch := createChannelFor(t, "limite_"+randHex(4))
	scopeAppViaRoute(t, e, owner, a.ID, ch.ID)
	scopeAppViaRoute(t, e, owner, b.ID, ch.ID)
	path := "/app/channels/" + ch.ID + "/messages"
	limited := false
	for i := 0; i < 100; i++ {
		rec := doWithBearer(t, e, http.MethodGet, path, nil, token)
		if rec.Code == 429 {
			limited = true
			break
		}
		if rec.Code != 200 {
			t.Fatal(rec.Body.String())
		}
	}
	if !limited {
		t.Fatal("burst não foi limitado")
	}
	if rec := doWithBearer(t, e, http.MethodGet, path, nil, other); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
}
