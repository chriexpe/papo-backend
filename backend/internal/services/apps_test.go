package services

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"papo/internal/models"
	"papo/internal/storage"
)

// newAppTest instala um bot com ambas as capacidades e um canal autorizado.
func newAppTest(t *testing.T) (models.AppInstallation, models.Channel, models.User) {
	t.Helper()
	u := newWebhookTestUser(t)
	ch := newWebhookTestChannel(t, "text")
	a, _, err := CreateApp(testCtx(), u.ID, "bot", "remote", "Bot", models.AppCapabilities{SendMessages: true, ReadMessages: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = AddAppScope(testCtx(), u.ID, a.ID, ch.ID); err != nil {
		t.Fatal(err)
	}
	return a, ch, u
}

// TestAppLifecycle cobre modos sem segredo, autenticação, identidade, rotação e revogação.
func TestAppLifecycle(t *testing.T) {
	u := newWebhookTestUser(t)
	for _, kind := range []string{"remote", "plugin"} {
		a, token, err := CreateApp(testCtx(), u.ID, "key", kind, "App", models.AppCapabilities{})
		if err != nil {
			t.Fatal(err)
		}
		var hash *string
		if err = storage.GetDB().QueryRowContext(testCtx(), "SELECT secret_hash FROM app_installations WHERE id=$1", a.ID).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		if kind != "remote" {
			if token != "" || hash != nil {
				t.Fatal("modo local com segredo")
			}
			if _, err = RotateAppToken(testCtx(), u.ID, a.ID); !errors.Is(err, ErrInvalidInput) {
				t.Fatal(err)
			}
			continue
		}
		if len(token) != 64 || hash == nil || *hash == token {
			t.Fatal("segredo não protegido")
		}
		if got, err := AuthenticateApp(testCtx(), token); err != nil || got.ID != a.ID {
			t.Fatalf("auth: %+v %v", got, err)
		}
		for _, bad := range []string{"", token + "x"} {
			if _, err = AuthenticateApp(testCtx(), bad); !errors.Is(err, ErrAppUnauthorized) {
				t.Fatal(err)
			}
		}
		avatar := base64.StdEncoding.EncodeToString(pngAvatarBytes(32, 32))
		name, display := "Novo", "Nickname"
		updated, err := UpdateApp(testCtx(), u.ID, a.ID, &name, &models.AppCapabilities{ReadMessages: true}, &display, &avatar, "png")
		if err != nil || updated.Name != name || updated.DisplayName == nil || *updated.DisplayName != display || !updated.Capabilities.ReadMessages {
			t.Fatalf("update: %+v %v", updated, err)
		}
		if _, _, err = GetAppAvatar(testCtx(), a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = UpdateApp(testCtx(), u.ID, a.ID, nil, nil, strPtr(""), strPtr(""), ""); err != nil {
			t.Fatal(err)
		}
		if _, _, err = GetAppAvatar(testCtx(), a.ID); !errors.Is(err, ErrAvatarNotFound) {
			t.Fatal(err)
		}
		rotated, err := RotateAppToken(testCtx(), u.ID, a.ID)
		if err != nil || rotated == token {
			t.Fatal(err)
		}
		if _, err = AuthenticateApp(testCtx(), token); !errors.Is(err, ErrAppUnauthorized) {
			t.Fatal(err)
		}
		if _, err = AuthenticateApp(testCtx(), rotated); err != nil {
			t.Fatal(err)
		}
		if err = RevokeApp(testCtx(), u.ID, a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = AuthenticateApp(testCtx(), rotated); !errors.Is(err, ErrAppUnauthorized) {
			t.Fatal(err)
		}
		if err = RevokeApp(testCtx(), u.ID, a.ID); !errors.Is(err, ErrAppNotFound) {
			t.Fatal(err)
		}
		if _, err = UpdateApp(testCtx(), u.ID, a.ID, nil, nil, nil, nil, ""); !errors.Is(err, ErrAppNotFound) {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"", strings.Repeat("n", 81), "injetado\n"} {
		if _, _, err := CreateApp(testCtx(), u.ID, "key", "remote", name, models.AppCapabilities{}); !errors.Is(err, ErrInvalidInput) {
			t.Fatal(err)
		}
	}
	if _, _, err := CreateApp(testCtx(), u.ID, "key", "other", "Nome", models.AppCapabilities{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	if _, err := activeApp(testCtx(), "inválido"); !errors.Is(err, ErrAppNotFound) {
		t.Fatal(err)
	}
	if _, _, err := GetAppAvatar(testCtx(), uuid.NewString()); !errors.Is(err, ErrAppNotFound) {
		t.Fatal(err)
	}
}

// TestAppScopeAndCapabilities cobre ACL humana na concessão e ACL de máquina por operação.
func TestAppScopeAndCapabilities(t *testing.T) {
	a, ch, u := newAppTest(t)
	role, err := storage.CreateRole(testCtx(), "priv_"+randHex(4), nil, models.RolePermissions{ManageApps: true})
	if err != nil {
		t.Fatal(err)
	}
	private := newWebhookTestChannel(t, "text")
	if _, err = storage.UpdateChannelPermissions(testCtx(), private.ID, role.ID, models.ChannelPermission{}); err != nil {
		t.Fatal(err)
	}
	if err = AddAppScope(testCtx(), u.ID, a.ID, private.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Fatal(err)
	}
	for _, target := range []models.Channel{private, newWebhookTestChannel(t, "dm"), newWebhookTestChannel(t, "text")} {
		if _, _, err = CreateAppMessage(testCtx(), a, target.ID, "", "negado", "", nil, nil); !errors.Is(err, ErrPermissionDenied) {
			t.Fatal(err)
		}
		if _, err = ListAppMessages(testCtx(), a, target.ID, nil, "", "", 0); !errors.Is(err, ErrPermissionDenied) {
			t.Fatal(err)
		}
	}
	if err = AddAppScope(testCtx(), u.ID, a.ID, newWebhookTestChannel(t, "dm").ID); !errors.Is(err, ErrPermissionDenied) {
		t.Fatal(err)
	}
	if err = AddAppScope(testCtx(), u.ID, a.ID, uuid.NewString()); !errors.Is(err, ErrChannelNotFound) {
		t.Fatal(err)
	}
	if _, err = UpdateApp(testCtx(), u.ID, a.ID, nil, &models.AppCapabilities{}, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err = CreateAppMessage(testCtx(), a, ch.ID, "", "negado", "", nil, nil); !errors.Is(err, ErrPermissionDenied) {
		t.Fatal(err)
	}
	if _, err = ListAppMessages(testCtx(), a, ch.ID, nil, "", "", 0); !errors.Is(err, ErrPermissionDenied) {
		t.Fatal(err)
	}
	if _, err = UpdateApp(testCtx(), u.ID, a.ID, nil, &a.Capabilities, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err = RemoveAppScope(testCtx(), u.ID, a.ID, ch.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = CreateAppMessage(testCtx(), a, ch.ID, "", "negado", "", nil, nil); !errors.Is(err, ErrPermissionDenied) {
		t.Fatal(err)
	}
	if err = AddAppScope(testCtx(), u.ID, a.ID, ch.ID); err != nil {
		t.Fatal(err)
	}
	if err = RevokeApp(testCtx(), u.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if has, err := storage.AppHasScope(testCtx(), a.ID, ch.ID); err != nil || has {
		t.Fatalf("escopo revogado: %v %v", has, err)
	}
	if _, _, err = CreateAppMessage(testCtx(), a, ch.ID, "", "negado", "", nil, nil); !errors.Is(err, ErrAppUnauthorized) {
		t.Fatal(err)
	}
}

// TestAppSenderMatrix garante isolamento entre humano, webhook e duas instalações.
func TestAppSenderMatrix(t *testing.T) {
	if err := cleanServers(testCtx()); err != nil {
		t.Fatal(err)
	}
	defer cleanServers(testCtx())
	a, ch, u := newAppTest(t)
	b, _, _ := newAppTest(t)
	if err := AddAppScope(testCtx(), u.ID, b.ID, ch.ID); err != nil {
		t.Fatal(err)
	}
	hook, _, err := CreateWebhook(testCtx(), u.ID, ch.ID, "Webhook")
	if err != nil {
		t.Fatal(err)
	}
	human, err := CreateMessage(testCtx(), ch.ID, u.ID, "humano", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	web, _, err := CreateWebhookMessage(testCtx(), hook, nil, "", "webhook", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	app, _, err := CreateAppMessage(testCtx(), a, ch.ID, "", "app", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{human.ID, web.ID, app.ID}
	edits := []func(string) error{
		func(id string) error { _, err := EditMessage(testCtx(), id, u.ID, "editada", nil); return err },
		func(id string) error { _, err := EditWebhookMessage(testCtx(), hook, id, "editada", nil); return err },
		func(id string) error { _, err := EditAppMessage(testCtx(), a, ch.ID, id, "editada", nil); return err },
	}
	deletes := []func(string) error{
		func(id string) error { _, err := DeleteMessage(testCtx(), id, u.ID); return err },
		func(id string) error { _, err := DeleteWebhookMessage(testCtx(), hook, id); return err },
		func(id string) error { _, err := DeleteAppMessage(testCtx(), a, ch.ID, id); return err },
	}
	for i := range ids {
		for j, id := range ids {
			if i == j {
				continue
			}
			if err := edits[i](id); !errors.Is(err, ErrPermissionDenied) {
				t.Fatalf("edit %d -> %d: %v", i, j, err)
			}
			if err := deletes[i](id); !errors.Is(err, ErrPermissionDenied) {
				t.Fatalf("delete %d -> %d: %v", i, j, err)
			}
		}
	}
	if _, err := EditAppMessage(testCtx(), b, ch.ID, app.ID, "roubo", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Fatal(err)
	}
	if _, err := DeleteAppMessage(testCtx(), b, ch.ID, app.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Fatal(err)
	}
	for i, id := range ids {
		if err := edits[i](id); err != nil {
			t.Fatal(err)
		}
	}
	moderator := newWebhookTestUser(t)
	role, err := storage.CreateRole(testCtx(), "mod_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.AssignUserRole(testCtx(), moderator.ID, role.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.UpdateChannelPermissions(testCtx(), ch.ID, role.ID, models.ChannelPermission{ReadChannel: true, DeleteMessages: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = DeleteMessage(testCtx(), app.ID, moderator.ID); err != nil {
		t.Fatal(err)
	}
}

// TestAppMessageRelations cobre retry completo, mídia, pins, busca, preview e @everyone.
func TestAppMessageRelations(t *testing.T) {
	if err := cleanServers(testCtx()); err != nil {
		t.Fatal(err)
	}
	defer cleanServers(testCtx())
	a, ch, u := newAppTest(t)
	if _, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &u.ID); err != nil {
		t.Fatal(err)
	}
	input := AttachmentInput{OriginalFileName: "arquivo.txt", Content: bytes.NewBufferString("anexo")}
	m, created, err := CreateAppMessage(testCtx(), a, ch.ID, "retry", "aviso @everyone", "", []AttachmentInput{input}, []EmbedInput{{Title: "Embed"}})
	if err != nil || !created {
		t.Fatal(err)
	}
	if m.AuthorID != nil || m.WebhookID != nil || m.AppInstallationID == nil || *m.AppInstallationID != a.ID || m.ExternalAuthor == nil || m.ExternalAuthor.AvatarURL != "/apps/"+a.ID+"/avatar" {
		t.Fatalf("atribuição: %+v", m)
	}
	retry, created, err := CreateAppMessage(testCtx(), a, ch.ID, "retry", "", "", nil, nil)
	if err != nil || created || retry.ID != m.ID || len(retry.Attachments) != 1 || len(retry.Embeds) != 1 {
		t.Fatalf("retry completo: %+v %v", retry, err)
	}
	var owner string
	if err = storage.GetDB().QueryRowContext(testCtx(), "SELECT created_by FROM attachments WHERE id=$1", m.Attachments[0].ID).Scan(&owner); err != nil || owner != u.ID {
		t.Fatalf("dono da mídia: %s %v", owner, err)
	}
	if _, _, err = PinMessage(testCtx(), ch.ID, m.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	pins, err := storage.ListPinnedMessagesWithAttachmentsByChannel(testCtx(), ch.ID)
	if err != nil || len(pins) != 1 || pins[0].AppInstallationID == nil || pins[0].ExternalAuthor == nil {
		t.Fatalf("pins: %+v %v", pins, err)
	}
	found, err := storage.SearchMessages(testCtx(), storage.SearchParams{UserID: u.ID, ChannelID: ch.ID})
	if err != nil || len(found) != 1 || found[0].AppInstallationID == nil || found[0].ExternalAuthor == nil {
		t.Fatalf("busca: %+v %v", found, err)
	}
	summary, err := storage.GetChannelSummary(testCtx(), ch.ID)
	if err != nil || summary.LastMessage == nil || summary.LastMessage.AppInstallationID == nil || summary.LastMessage.ExternalAuthor == nil {
		t.Fatalf("preview: %+v %v", summary, err)
	}
	history, err := ListAppMessages(testCtx(), a, ch.ID, nil, "", "asc", 1)
	if err != nil || len(history.Messages) != 1 || len(history.Messages[0].Embeds) != 1 {
		t.Fatalf("histórico: %+v %v", history, err)
	}
	if _, err = ListAppMessages(testCtx(), a, ch.ID, nil, "", "inválido", 1); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	if deliveries := DispatchMessageNotifications(testCtx(), "app-test", m.Message); len(deliveries) != 0 {
		t.Fatalf("app disparou @everyone: %+v", deliveries)
	}
	var actor *string
	var label, metadata string
	if err = storage.GetDB().QueryRowContext(testCtx(), "SELECT actor_id,actor_username,metadata->>'app_installation_id' FROM audit_logs WHERE action='message.create' AND entity_id=$1", m.ID).Scan(&actor, &label, &metadata); err != nil || actor != nil || label != "app:Bot" || metadata != a.ID {
		t.Fatalf("audit: %v %q %q %v", actor, label, metadata, err)
	}
	if _, err = DeleteAppMessage(testCtx(), a, ch.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	after, created, err := CreateAppMessage(testCtx(), a, ch.ID, "retry", "após exclusão", "", nil, nil)
	if err != nil || !created || after.ID == m.ID {
		t.Fatalf("chave expira com a mensagem: %v", err)
	}
}

// TestAppPagination cobre cursores compostos nas duas ordens e validação de campos.
func TestAppPagination(t *testing.T) {
	u := newWebhookTestUser(t)
	for i := 0; i < 4; i++ {
		if _, _, err := CreateApp(testCtx(), u.ID, "key", "remote", "Página", models.AppCapabilities{}); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := storage.GetDB().ExecContext(testCtx(), "UPDATE app_installations SET created_at=$1", stamp); err != nil {
		t.Fatal(err)
	}
	for _, order := range []string{"asc", "desc"} {
		seen := map[string]bool{}
		var since *time.Time
		last := ""
		for {
			page, err := ListApps(testCtx(), since, last, order, 2)
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range page.Apps {
				if seen[a.ID] {
					t.Fatal("app repetido")
				}
				seen[a.ID] = true
			}
			if !page.HasMore {
				break
			}
			a := page.Apps[len(page.Apps)-1]
			since = &a.CreatedAt
			last = a.ID
		}
		if len(seen) < 4 {
			t.Fatal("perdeu apps")
		}
	}
	if _, err := ListApps(testCtx(), nil, "", "errado", 1); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	if _, err := ListApps(testCtx(), &stamp, "errado", "", 1); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	a, _, _ := newAppTest(t)
	for _, bad := range []string{"", strings.Repeat("n", 81)} {
		if _, err := UpdateApp(testCtx(), u.ID, a.ID, &bad, nil, nil, nil, ""); !errors.Is(err, ErrInvalidInput) {
			t.Fatal(err)
		}
	}
	if _, err := UpdateApp(testCtx(), u.ID, a.ID, nil, nil, nil, strPtr("inválido"), "png"); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
}

// TestAppIdempotencyDoesNotCrossChannels impede que um retry vaze mensagens de outro escopo.
func TestAppIdempotencyDoesNotCrossChannels(t *testing.T) {
	a, ch, u := newAppTest(t)
	other := newWebhookTestChannel(t, "text")
	if err := AddAppScope(testCtx(), u.ID, a.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	m, _, err := CreateAppMessage(testCtx(), a, ch.ID, "global-key", "segredo", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = RemoveAppScope(testCtx(), u.ID, a.ID, ch.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = CreateAppMessage(testCtx(), a, other.ID, "global-key", "retry", "", nil, nil); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("chave de outro canal revelou %s: %v", m.ID, err)
	}
}

// TestMessageSenderInvariant rejeita zero ou múltiplos principais antes de gravar mídia.
func TestMessageSenderInvariant(t *testing.T) {
	for _, s := range []messageSender{{}, {authorID: "human", webhookID: "hook"}, {webhookID: "hook", appInstallationID: "app"}, {authorID: "human", appInstallationID: "app"}} {
		if _, _, err := createMessageAs(testCtx(), "channel", s, nil, "", "content", "", nil, nil); !errors.Is(err, ErrInvalidInput) {
			t.Fatal(err)
		}
	}
}
