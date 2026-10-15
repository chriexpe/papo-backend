package services

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"papo/internal/models"
	"papo/internal/storage"
)

// TestWebhookManagementChannelACL cobre dono, canais abertos e ACL por role na gestão.
func TestWebhookManagementChannelACL(t *testing.T) {
	if err := cleanServers(testCtx()); err != nil {
		t.Fatal(err)
	}
	defer cleanServers(testCtx())
	owner, actor := newWebhookTestUser(t), newWebhookTestUser(t)
	if _, err := storage.CreateServer(testCtx(), "srv_"+randHex(4), &owner.ID); err != nil {
		t.Fatal(err)
	}
	role, err := storage.CreateRole(testCtx(), "role_"+randHex(4), nil, models.RolePermissions{ManageWebhooks: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.AssignUserRole(testCtx(), actor.ID, role.ID); err != nil {
		t.Fatal(err)
	}
	channel := newWebhookTestChannel(t, "text")
	if _, err := storage.UpdateChannelPermissions(testCtx(), channel.ID, role.ID, models.ChannelPermission{ReadChannel: false}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CreateWebhook(testCtx(), actor.ID, channel.ID, "negado"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("criação privada: %v", err)
	}
	hook, _, err := CreateWebhook(testCtx(), owner.ID, channel.ID, "dono")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateWebhookIdentity(testCtx(), actor.ID, hook.ID, strPtr("negado"), nil, ""); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("PATCH privado: %v", err)
	}
	if err := RevokeWebhook(testCtx(), actor.ID, hook.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("DELETE privado: %v", err)
	}
	unchanged, err := storage.GetWebhookByID(testCtx(), hook.ID)
	if err != nil || unchanged.DisplayName != nil || unchanged.RevokedAt != nil {
		t.Fatalf("negação alterou estado: %+v %v", unchanged, err)
	}
	page, err := ListWebhooks(testCtx(), actor.ID, nil, "", "", 0)
	if err != nil || len(page.Webhooks) != 0 || page.Webhooks == nil || page.HasMore {
		t.Fatalf("lista privada: %+v %v", page, err)
	}
	page, err = ListWebhooks(testCtx(), owner.ID, nil, "", "", 0)
	if err != nil || len(page.Webhooks) != 1 {
		t.Fatalf("lista dono: %+v %v", page, err)
	}
	if _, err := UpdateWebhookIdentity(testCtx(), owner.ID, hook.ID, strPtr("dono"), nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.UpdateChannelPermissions(testCtx(), channel.ID, role.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CreateWebhook(testCtx(), actor.ID, channel.ID, "role permitida"); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateWebhookIdentity(testCtx(), actor.ID, hook.ID, strPtr("permitido"), nil, ""); err != nil {
		t.Fatal(err)
	}
	page, err = ListWebhooks(testCtx(), actor.ID, nil, "", "", 0)
	if err != nil || len(page.Webhooks) != 2 {
		t.Fatalf("lista role: %+v %v", page, err)
	}
	if err := RevokeWebhook(testCtx(), actor.ID, hook.ID); err != nil {
		t.Fatal(err)
	}
	if err := RevokeWebhook(testCtx(), actor.ID, hook.ID); !errors.Is(err, ErrWebhookNotFound) {
		t.Fatalf("revogado: %v", err)
	}
	if _, err := UpdateWebhookIdentity(testCtx(), actor.ID, hook.ID, nil, nil, ""); !errors.Is(err, ErrWebhookNotFound) {
		t.Fatalf("editar revogado: %v", err)
	}
	for _, kind := range []string{"text", "voice"} {
		open := newWebhookTestChannel(t, kind)
		h, _, err := CreateWebhook(testCtx(), actor.ID, open.ID, "aberto")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := UpdateWebhookIdentity(testCtx(), actor.ID, h.ID, nil, nil, ""); err != nil {
			t.Fatal(err)
		}
		if err := RevokeWebhook(testCtx(), actor.ID, h.ID); err != nil {
			t.Fatal(err)
		}
	}
	dm := newWebhookTestChannel(t, "dm")
	if _, _, err := CreateWebhook(testCtx(), owner.ID, dm.ID, "dm"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("dm: %v", err)
	}
	if _, _, err := CreateWebhook(testCtx(), actor.ID, uuid.NewString(), "ausente"); !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("canal ausente: %v", err)
	}
}

// TestWebhookManagementPaginationAndCap cobre limite, revogação e paginação sem perdas.
func TestWebhookManagementPaginationAndCap(t *testing.T) {
	if err := cleanServers(testCtx()); err != nil {
		t.Fatal(err)
	}
	defer cleanServers(testCtx())
	user := newWebhookTestUser(t)
	channel := newWebhookTestChannel(t, "text")
	var first models.Webhook
	for i := 0; i < storage.MaxActiveWebhooksPerChannel; i++ {
		hook, _, err := CreateWebhook(testCtx(), user.ID, channel.ID, "paginação")
		if err != nil {
			t.Fatal(err)
		}
		first = hook
	}
	if _, _, err := CreateWebhook(testCtx(), user.ID, channel.ID, "excesso"); !errors.Is(err, ErrWebhookLimitReached) {
		t.Fatalf("limite: %v", err)
	}
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := storage.GetDB().ExecContext(testCtx(), "UPDATE webhooks SET created_at=$1", stamp); err != nil {
		t.Fatal(err)
	}
	defaults, err := ListWebhooks(testCtx(), user.ID, nil, "", "", 0)
	if err != nil || len(defaults.Webhooks) != 30 || !defaults.HasMore {
		t.Fatalf("padrão: %+v %v", defaults, err)
	}
	for _, order := range []string{"asc", "desc"} {
		seen := map[string]bool{}
		var since *time.Time
		lastID := ""
		for {
			page, err := ListWebhooks(testCtx(), user.ID, since, lastID, order, 7)
			if err != nil {
				t.Fatal(err)
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
			since = &last.CreatedAt
			lastID = last.ID
		}
		if len(seen) != 50 {
			t.Fatalf("ordem %s perdeu itens: %d", order, len(seen))
		}
	}
	if _, err := ListWebhooks(testCtx(), user.ID, nil, "", "inválida", 1); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ordem: %v", err)
	}
	if _, err := ListWebhooks(testCtx(), user.ID, &stamp, "inválido", "", 1); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("cursor: %v", err)
	}
	if err := RevokeWebhook(testCtx(), user.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CreateWebhook(testCtx(), user.ID, channel.ID, "vaga"); err != nil {
		t.Fatal(err)
	}
}
