package storage

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"papo/internal/models"
)

// TestWebhookStorageLifecycle cobre persistência, identidade, avatar, mensagens e revogação.
func TestWebhookStorageLifecycle(t *testing.T) {
	wipeAppTables(t)
	user := newTestUser(t)
	channel := newTestChannel(t)
	hook, err := CreateWebhook(testCtx(), channel.ID, "integração", user.ID, "hash-secreto")
	if err != nil {
		t.Fatal(err)
	}
	got, hash, err := GetActiveWebhookWithHash(testCtx(), hook.ID)
	if err != nil || hash != "hash-secreto" || got.ID != hook.ID {
		t.Fatalf("hash/registro: %+v %q %v", got, hash, err)
	}
	if _, err := GetWebhookByID(testCtx(), hook.ID); err != nil {
		t.Fatal(err)
	}
	avatar, err := GetWebhookAvatarMedia(testCtx(), hook.ID)
	if err != nil || avatar != nil {
		t.Fatalf("avatar inicial: %v %v", avatar, err)
	}
	media := newTestMedia(t, []byte("avatar"))
	updated, err := UpdateWebhookIdentity(testCtx(), hook.ID, sql.NullString{String: "Bot", Valid: true}, sql.NullString{String: media, Valid: true})
	if err != nil || updated.DisplayName == nil || *updated.DisplayName != "Bot" {
		t.Fatalf("identidade: %+v %v", updated, err)
	}
	avatar, err = GetWebhookAvatarMedia(testCtx(), hook.ID)
	if err != nil || avatar == nil || *avatar != media {
		t.Fatalf("avatar: %v %v", avatar, err)
	}
	preserved, err := UpdateWebhookIdentity(testCtx(), hook.ID, nil, nil)
	if err != nil || preserved.AvatarMedia == nil {
		t.Fatalf("preservar: %+v %v", preserved, err)
	}
	updated, err = UpdateWebhookIdentity(testCtx(), hook.ID, sql.NullString{}, sql.NullString{})
	if err != nil || updated.DisplayName != nil || updated.AvatarMedia != nil {
		t.Fatalf("limpar: %+v %v", updated, err)
	}
	message, err := CreateMessage(testCtx(), channel.ID, user.ID, "atribuição", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GetDB().ExecContext(testCtx(), "UPDATE messages SET author_id=NULL, webhook_id=$1, external_message_id='origem' WHERE id=$2", hook.ID, message.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := GetWebhookMessage(testCtx(), hook.ID, "origem")
	if err != nil || stored.ID != message.ID {
		t.Fatalf("mensagem: %+v %v", stored, err)
	}
	if _, err := GetWebhookMessage(testCtx(), hook.ID, "ausente"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mensagem ausente: %v", err)
	}
	if err := RevokeWebhook(testCtx(), hook.ID); err != nil {
		t.Fatal(err)
	}
	if err := RevokeWebhook(testCtx(), hook.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revogação repetida: %v", err)
	}
	got, err = GetWebhookByID(testCtx(), hook.ID)
	if err != nil || got.RevokedAt == nil {
		t.Fatalf("registro revogado: %+v %v", got, err)
	}
	if _, _, err := GetActiveWebhookWithHash(testCtx(), hook.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ativo revogado: %v", err)
	}
	missing := uuid.NewString()
	if _, err := GetWebhookByID(testCtx(), missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ausente: %v", err)
	}
	if _, err := GetWebhookAvatarMedia(testCtx(), missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("avatar ausente: %v", err)
	}
	if _, err := UpdateWebhookIdentity(testCtx(), missing, "nome", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("atualização ausente: %v", err)
	}
	if err := RevokeWebhook(testCtx(), missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revogação ausente: %v", err)
	}
	if _, err := CreateWebhook(testCtx(), missing, "x", user.ID, "hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("canal ausente: %v", err)
	}
}

// TestWebhookStoragePaginationACL cobre ACL antes do limite e cursores empatados nas duas ordens.
func TestWebhookStoragePaginationACL(t *testing.T) {
	wipeAppTables(t)
	owner, actor := newTestUser(t), newTestUser(t)
	newTestServer(t, &owner.ID)
	open, private := newTestChannel(t), newTestChannel(t)
	role := newTestRole(t)
	if _, err := UpdateChannelPermissions(testCtx(), private.ID, role.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
		t.Fatal(err)
	}
	hooks := make([]models.Webhook, 0)
	for i := 0; i < 3; i++ {
		hook, err := CreateWebhook(testCtx(), open.ID, "aberto", owner.ID, "hash")
		if err != nil {
			t.Fatal(err)
		}
		hooks = append(hooks, hook)
	}
	hidden, err := CreateWebhook(testCtx(), private.ID, "privado", owner.ID, "hash")
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := GetDB().ExecContext(testCtx(), "UPDATE webhooks SET created_at=$1", stamp); err != nil {
		t.Fatal(err)
	}
	for _, asc := range []bool{false, true} {
		first, err := ListWebhooks(testCtx(), actor.ID, nil, "", asc, 2)
		if err != nil || len(first) != 2 {
			t.Fatalf("primeira página: %+v %v", first, err)
		}
		second, err := ListWebhooks(testCtx(), actor.ID, &stamp, first[1].ID, asc, 2)
		if err != nil || len(second) != 1 || second[0].ID == first[0].ID || second[0].ID == first[1].ID {
			t.Fatalf("segunda página: %+v %v", second, err)
		}
		for _, h := range append(first, second...) {
			if h.ID == hidden.ID {
				t.Fatal("webhook privado vazou")
			}
		}
		onlySince, err := ListWebhooks(testCtx(), actor.ID, &stamp, "", asc, 10)
		if err != nil || len(onlySince) != 0 {
			t.Fatalf("since estrito: %+v %v", onlySince, err)
		}
	}
	// since isolado é polling, inclusive em ordem decrescente; last_id isolado é ignorado.
	before := stamp.Add(-time.Second)
	polling, err := ListWebhooks(testCtx(), actor.ID, &before, "", false, 10)
	if err != nil || len(polling) != 3 {
		t.Fatalf("polling desc: %+v %v", polling, err)
	}
	ignored, err := ListWebhooks(testCtx(), actor.ID, nil, "inválido", false, 10)
	if err != nil || len(ignored) != 3 {
		t.Fatalf("last_id isolado: %+v %v", ignored, err)
	}

	ownerPage, err := ListWebhooks(testCtx(), owner.ID, nil, "", false, 10)
	if err != nil || len(ownerPage) != 4 {
		t.Fatalf("dono: %+v %v", ownerPage, err)
	}
	if _, err := AssignUserRole(testCtx(), actor.ID, role.ID); err != nil {
		t.Fatal(err)
	}
	allowed, err := ListWebhooks(testCtx(), actor.ID, nil, "", false, 10)
	if err != nil || len(allowed) != 4 {
		t.Fatalf("role: %+v %v", allowed, err)
	}
	if err := RevokeWebhook(testCtx(), hidden.ID); err != nil {
		t.Fatal(err)
	}
	allowed, err = ListWebhooks(testCtx(), actor.ID, nil, "", false, 10)
	if err != nil || len(allowed) != 3 {
		t.Fatalf("revogado: %+v %v", allowed, err)
	}
	if err := RevokeWebhook(testCtx(), hooks[0].ID); err != nil {
		t.Fatal(err)
	}
}

// TestWebhookStorageConcurrentCap garante que criações simultâneas não ultrapassam o teto.
func TestWebhookStorageConcurrentCap(t *testing.T) {
	wipeAppTables(t)
	user := newTestUser(t)
	channel, other := newTestChannel(t), newTestChannel(t)
	var first models.Webhook
	for i := 0; i < MaxActiveWebhooksPerChannel-1; i++ {
		hook, err := CreateWebhook(testCtx(), channel.ID, "hook", user.ID, "hash")
		if err != nil {
			t.Fatal(err)
		}
		first = hook
	}
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := CreateWebhook(testCtx(), channel.ID, "concorrente", user.ID, "hash")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrWebhookLimitReached) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("esperava uma vaga preenchida, obtive %d", successes)
	}
	if _, err := CreateWebhook(testCtx(), other.ID, "outro canal", user.ID, "hash"); err != nil {
		t.Fatal(err)
	}
	if err := RevokeWebhook(testCtx(), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateWebhook(testCtx(), channel.ID, "vaga liberada", user.ID, "hash"); err != nil {
		t.Fatal(err)
	}
}
