package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/storage"
)

// TestWebhookManagementReadChannelRoutes cobre as quatro rotas com ACL negada e permitida.
func TestWebhookManagementReadChannelRoutes(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	ownerToken := webhookTestServer(t, e)
	actorID, actorToken := registerAndLogin(t, e)
	role := createRoleFor(t, models.RolePermissions{ManageWebhooks: true})
	assignRoleToUser(t, actorID, role.ID)
	channel := createChannelFor(t, "privado_"+randHex(4))
	if _, err := storage.UpdateChannelPermissions(context.Background(), channel.ID, role.ID, models.ChannelPermission{}); err != nil {
		t.Fatal(err)
	}
	hook, _ := createWebhookViaRoute(t, e, ownerToken, channel.ID, "privado")
	body, _ := json.Marshal(map[string]string{"channel_id": channel.ID, "name": "negado"})
	assertProblem(t, do(t, e, http.MethodPost, "/webhooks", body, authCookie(actorToken)), 403, "forbidden", "Acesso negado", "operação não permitida")
	assertProblem(t, do(t, e, http.MethodPatch, "/webhooks/"+hook.ID, []byte(`{"display_name":"negado"}`), authCookie(actorToken)), 403, "forbidden", "Acesso negado", "operação não permitida")
	assertProblem(t, do(t, e, http.MethodDelete, "/webhooks/"+hook.ID, nil, authCookie(actorToken)), 403, "forbidden", "Acesso negado", "operação não permitida")
	rec := do(t, e, http.MethodGet, "/webhooks", nil, authCookie(actorToken))
	var page models.WebhookList
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &page) != nil || page.Webhooks == nil || len(page.Webhooks) != 0 || page.HasMore {
		t.Fatalf("lista negada: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, e, http.MethodGet, "/webhooks", nil, authCookie(ownerToken))
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &page) != nil || len(page.Webhooks) != 1 {
		t.Fatalf("lista dono: %s", rec.Body.String())
	}
	rec = do(t, e, http.MethodPatch, "/webhooks/"+hook.ID, []byte(`{"display_name":"dono"}`), authCookie(ownerToken))
	if rec.Code != 200 {
		t.Fatalf("PATCH dono: %s", rec.Body.String())
	}
	if _, err := storage.UpdateChannelPermissions(context.Background(), channel.ID, role.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
		t.Fatal(err)
	}
	createWebhookViaRoute(t, e, actorToken, channel.ID, "role")
	rec = do(t, e, http.MethodPatch, "/webhooks/"+hook.ID, []byte(`{"display_name":"permitido"}`), authCookie(actorToken))
	if rec.Code != 200 {
		t.Fatalf("PATCH permitido: %s", rec.Body.String())
	}
	rec = do(t, e, http.MethodDelete, "/webhooks/"+hook.ID, nil, authCookie(actorToken))
	if rec.Code != 204 {
		t.Fatalf("DELETE permitido: %s", rec.Body.String())
	}
	rec = do(t, e, http.MethodGet, "/webhooks", nil, authCookie(actorToken))
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &page) != nil || len(page.Webhooks) != 1 {
		t.Fatalf("lista permitida: %s", rec.Body.String())
	}
	// Perder read_channel passa a bloquear imediatamente todas as operações.
	if _, err := storage.UpdateChannelPermissions(context.Background(), channel.ID, role.ID, models.ChannelPermission{}); err != nil {
		t.Fatal(err)
	}
	remaining := page.Webhooks[0]
	assertProblem(t, do(t, e, http.MethodPatch, "/webhooks/"+remaining.ID, []byte(`{}`), authCookie(actorToken)), 403, "forbidden", "Acesso negado", "operação não permitida")
	assertProblem(t, do(t, e, http.MethodDelete, "/webhooks/"+remaining.ID, nil, authCookie(actorToken)), 403, "forbidden", "Acesso negado", "operação não permitida")
	rec = do(t, e, http.MethodGet, "/webhooks?limit=1", nil, authCookie(actorToken))
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &page) != nil || len(page.Webhooks) != 0 || page.HasMore {
		t.Fatalf("lista após perda de leitura: %s", rec.Body.String())
	}
	if rec := do(t, e, http.MethodDelete, "/webhooks/"+remaining.ID, nil, authCookie(ownerToken)); rec.Code != 204 {
		t.Fatalf("DELETE dono: %s", rec.Body.String())
	}
	for _, kind := range []string{"voice", "dm"} {
		ch, err := storage.CreateChannel(context.Background(), kind+"_"+randHex(4), kind, "")
		if err != nil {
			t.Fatal(err)
		}
		if kind == "voice" {
			createWebhookViaRoute(t, e, actorToken, ch.ID, "voz")
		} else {
			payload, _ := json.Marshal(map[string]string{"channel_id": ch.ID, "name": "dm"})
			assertProblem(t, do(t, e, http.MethodPost, "/webhooks", payload, authCookie(ownerToken)), 403, "forbidden", "Acesso negado", "operação não permitida")
		}
	}

	_, plainToken := registerAndLogin(t, e)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		path := "/webhooks"
		if method == http.MethodPatch || method == http.MethodDelete {
			path += "/" + hook.ID
		}
		assertProblem(t, do(t, e, method, path, body, authCookie(plainToken)), 403, "forbidden", "Acesso negado", "usuário não possui a permissão necessária para esta operação")
	}

	open := createChannelFor(t, "aberto_"+randHex(4))
	h, _ := createWebhookViaRoute(t, e, actorToken, open.ID, "aberto")
	if rec := do(t, e, http.MethodPatch, "/webhooks/"+h.ID, []byte(`{}`), authCookie(actorToken)); rec.Code != 200 {
		t.Fatalf("PATCH aberto: %s", rec.Body.String())
	}
	if rec := do(t, e, http.MethodDelete, "/webhooks/"+h.ID, nil, authCookie(actorToken)); rec.Code != 204 {
		t.Fatalf("DELETE aberto: %s", rec.Body.String())
	}
}

// TestWebhookManagementPaginationCapRoutes cobre paginação, erros de query e o teto ativo por canal.
func TestWebhookManagementPaginationCapRoutes(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	token := webhookTestServer(t, e)
	channel := createChannelFor(t, "paginas_"+randHex(4))
	var first models.Webhook
	for i := 0; i < storage.MaxActiveWebhooksPerChannel; i++ {
		first, _ = createWebhookViaRoute(t, e, token, channel.ID, "hook")
	}
	body, _ := json.Marshal(map[string]string{"channel_id": channel.ID, "name": "excesso"})
	assertProblem(t, do(t, e, http.MethodPost, "/webhooks", body, authCookie(token)), 400, "invalid-param", "Parâmetro inválido", services.ErrWebhookLimitReached.Error())
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := storage.GetDB().ExecContext(context.Background(), "UPDATE webhooks SET created_at=$1", stamp); err != nil {
		t.Fatal(err)
	}
	for _, order := range []string{"asc", "desc"} {
		path := "/webhooks?limit=7&order=" + order
		seen := map[string]bool{}
		for {
			rec := do(t, e, http.MethodGet, path, nil, authCookie(token))
			var page models.WebhookList
			if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &page) != nil {
				t.Fatalf("página: %d %s", rec.Code, rec.Body.String())
			}
			for _, h := range page.Webhooks {
				if seen[h.ID] {
					t.Fatal("item repetido")
				}
				seen[h.ID] = true
			}
			if !page.HasMore {
				break
			}
			last := page.Webhooks[len(page.Webhooks)-1]
			path = "/webhooks?limit=7&order=" + order + "&since=" + url.QueryEscape(last.CreatedAt.Format(time.RFC3339Nano)) + "&last_id=" + last.ID
		}
		if len(seen) != 50 {
			t.Fatalf("ordem %s: %d itens", order, len(seen))
		}
	}

	for _, tc := range []struct{ query, detail string }{
		{"since=inválido", "since deve ser um timestamp ISO 8601"},
		{"limit=0", "limit deve ser um inteiro entre 1 e 100"},
		{"limit=101", "limit deve ser um inteiro entre 1 e 100"},
		{"limit=x", "limit deve ser um inteiro entre 1 e 100"},
		{"order=x", services.ErrInvalidInput.Error()},
		{"since=2026-01-01T00:00:00Z&last_id=x", services.ErrInvalidInput.Error()},
	} {
		assertProblem(t, do(t, e, http.MethodGet, "/webhooks?"+tc.query, nil, authCookie(token)),
			http.StatusBadRequest, "invalid-param", "Parâmetro inválido", tc.detail)
	}

	rec := do(t, e, http.MethodGet, "/webhooks", nil, authCookie(token))
	var page models.WebhookList
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &page) != nil || len(page.Webhooks) != 30 || !page.HasMore {
		t.Fatalf("padrão: %s", rec.Body.String())
	}
	if rec := do(t, e, http.MethodDelete, "/webhooks/"+first.ID, nil, authCookie(token)); rec.Code != 204 {
		t.Fatalf("revogar: %s", rec.Body.String())
	}
	createWebhookViaRoute(t, e, token, channel.ID, "vaga liberada")
	other := createChannelFor(t, "outro_"+randHex(4))
	createWebhookViaRoute(t, e, token, other.ID, "outro canal")
}
