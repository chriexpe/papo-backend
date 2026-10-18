package storage

import (
	"os"
	"papo/internal/models"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppInteractionsMigrationRoundtrip preserva notificações de mensagens em down/up.
func TestAppInteractionsMigrationRoundtrip(t *testing.T) {
	ctx := testCtx()
	u := newTestUser(t)
	ch := newTestChannel(t)
	msg, e := CreateMessage(ctx, ch.ID, u.ID, "notificação anterior", "", nil)
	if e != nil {
		t.Fatal(e)
	}
	before, e := CreateNotification(ctx, u.ID, msg.ID)
	if e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(filepath.Join(migrationsDir, "022_app_plugins.sql"))
	if e != nil {
		t.Fatal(e)
	}
	parts := strings.SplitN(string(data), "-- +goose Down", 2)
	if len(parts) != 2 {
		t.Fatal("migration sem down")
	}
	if _, e = GetDB().ExecContext(ctx, parts[1]); e != nil {
		t.Fatal("down", e)
	}
	var n int
	if e = GetDB().QueryRowContext(ctx, `SELECT count(*) FROM notifications WHERE id=$1 AND message_id=$2`, before.ID, msg.ID).Scan(&n); e != nil || n != 1 {
		t.Fatal("notificação perdida", n, e)
	}
	if _, e = GetDB().ExecContext(ctx, parts[0]); e != nil {
		t.Fatal("up", e)
	}
	var kind string
	if e = GetDB().QueryRowContext(ctx, `SELECT kind FROM notifications WHERE id=$1`, before.ID).Scan(&kind); e != nil || kind != "message" {
		t.Fatal(kind, e)
	}
}

// TestAppInteractionCleanup remove tokens, convites e push expirados sem apagar mensagens.
func TestAppInteractionCleanup(t *testing.T) {
	ctx := testCtx()
	u := newTestUser(t)
	v := newTestUser(t)
	ch := newTestChannel(t)
	a, e := CreateApp(ctx, "cleanup", "plugin", "Limpeza", u.ID, models.AppCapabilities{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	_, e = GetDB().ExecContext(ctx, `INSERT INTO notifications(user_id,kind,app_installation_id,payload,expires_at,invite_status) VALUES($1,'app_invite',$2,'{}',now()-interval '1 second','pending')`, v.ID, a.ID)
	if e != nil {
		t.Fatal(e)
	}
	_, e = GetDB().ExecContext(ctx, `INSERT INTO app_ui_tokens(token_hash,kind,user_id,installation_id,channel_id,surface_id,generation,digest,expires_at,absolute_expires_at) VALUES('expired-cleanup','ticket',$1,$2,$3,'home',0,'digest',now()-interval '1 second',now())`, u.ID, a.ID, ch.ID)
	if e != nil {
		t.Fatal(e)
	}
	if e = CleanAppInteractions(ctx); e != nil {
		t.Fatal(e)
	}
	var n int
	if e = GetDB().QueryRow(`SELECT count(*) FROM app_ui_tokens WHERE token_hash='expired-cleanup'`).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
	if e = GetDB().QueryRow(`SELECT count(*) FROM notifications WHERE app_installation_id=$1`, a.ID).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}

// TestMessageComponentsInEditTransaction grava os botões da edição na mesma
// transação: lista vazia remove, contexto sem botões preserva os atuais.
func TestMessageComponentsInEditTransaction(t *testing.T) {
	ctx := testCtx()
	u := newTestUser(t)
	ch := newTestChannel(t)
	buttons := [][]models.AppButton{{{ID: "jogar", Label: "Jogar", Style: "primary"}}}
	msg, err := CreateMessage(WithMessageComponents(ctx, buttons), ch.ID, u.ID, "com botões", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	components := func() string {
		var raw string
		if err := GetDB().QueryRowContext(ctx, "SELECT components::text FROM messages WHERE id=$1", msg.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if !strings.Contains(components(), "jogar") {
		t.Fatal("criação sem botões", components())
	}
	content := "editada"
	if _, err := UpdateMessage(ctx, msg.ID, models.Message{Content: &content}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(components(), "jogar") {
		t.Fatal("edição sem botões no contexto apagou os atuais", components())
	}
	if _, err := UpdateMessage(WithMessageComponents(ctx, [][]models.AppButton{}), msg.ID, models.Message{Content: &content}); err != nil {
		t.Fatal(err)
	}
	if got := components(); got != "[]" {
		t.Fatal("lista vazia deveria remover os botões", got)
	}
}
