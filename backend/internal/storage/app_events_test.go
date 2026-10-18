package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"papo/internal/models"
)

// appEventFixture isola o backlog e instala um leitor no canal criado para o teste.
func appEventFixture(t *testing.T) (models.AppInstallation, models.Channel, models.User) {
	t.Helper()
	wipeAppTables(t)
	if _, err := GetDB().Exec("TRUNCATE app_event_outbox CASCADE"); err != nil {
		t.Fatal(err)
	}
	user := newTestUser(t)
	channel := newTestChannel(t)
	hash := uuid.NewString()
	app, err := CreateApp(testCtx(), "events", "remote", "Bot", user.ID, models.AppCapabilities{ReadMessages: true}, &hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddAppScope(testCtx(), app.ID, channel.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	return app, channel, user
}

// eventMessage cria uma mensagem com erro fatal para deixar os cenários legíveis.
func eventMessage(t *testing.T, channel models.Channel, user models.User, content string) models.Message {
	t.Helper()
	m, err := CreateMessage(testCtx(), channel.ID, user.ID, content, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// dispatchEvents exige que as duas fases de produção e atribuição tenham terminado.
func dispatchEvents(t *testing.T) {
	t.Helper()
	if _, err := DispatchAppEvents(testCtx(), 1000, 10000); err != nil {
		t.Fatal(err)
	}
}

// eventDeliveries lê as entregas com a autorização atual, sem uma conexão humana.
func eventDeliveries(t *testing.T, id string) []AppEventDelivery {
	t.Helper()
	ds, err := ListAppEventDeliveries(testCtx(), id, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

// TestAppEventsSnapshots cobre snapshots, anexos, autoria e conflitos idempotentes.
func TestAppEventsSnapshots(t *testing.T) {
	app, ch, user := appEventFixture(t)
	media := newTestMedia(t, []byte("app-event-file"))
	attachment, err := CreateAttachment(testCtx(), models.Attachments{OriginalFileName: "arquivo.txt", MediaShaHash: media, CreatedBy: &user.ID})
	if err != nil {
		t.Fatal(err)
	}
	original, created, err := CreateAppMessage(testCtx(), ch.ID, app.ID, &models.ExternalAuthor{Source: "app", ID: app.ID, Name: "Bot"}, "dedupe", "hello", "", []string{attachment.ID})
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if _, created, err := CreateAppMessage(testCtx(), ch.ID, app.ID, nil, "dedupe", "ignored", "", nil); err != nil || created {
		t.Fatalf("retry: %v %v", created, err)
	}
	if _, err := UpdateMessage(testCtx(), original.ID, models.Message{Content: strPtr("edit")}); err != nil {
		t.Fatal(err)
	}
	unicode := "👍"
	if _, _, _, err := AddReaction(testCtx(), original.ID, user.ID, nil, &unicode); err != nil {
		t.Fatal(err)
	}
	if _, created, _, err := AddReaction(testCtx(), original.ID, user.ID, nil, &unicode); err != nil || created {
		t.Fatalf("retry reaction: %v %v", created, err)
	}
	if _, err := RemoveReaction(testCtx(), original.ID, user.ID, nil, &unicode); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveReaction(testCtx(), original.ID, user.ID, nil, &unicode); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	dispatchEvents(t)
	ds := eventDeliveries(t, app.ID)
	if len(ds) != 4 {
		t.Fatalf("deliveries=%+v", ds)
	}
	var payload map[string]any
	if err := json.Unmarshal(ds[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["content"] != "hello" || payload["sender_kind"] != "app" || payload["sender_id"] != app.ID {
		t.Fatalf("snapshot=%s", ds[0].Payload)
	}
	if _, ok := payload["embeds"]; ok {
		t.Fatal("embeds no snapshot")
	}
	attachments := payload["attachments"].([]any)
	if len(attachments) != 1 || attachments[0].(map[string]any)["id"] != attachment.ID {
		t.Fatalf("attachments=%v", attachments)
	}
	for i, typ := range []string{"message.created", "message.updated", "reaction.added", "reaction.removed"} {
		if ds[i].Type != typ || ds[i].DeliverySeq != int64(i+1) {
			t.Fatalf("entrega=%+v", ds[i])
		}
	}
}

// TestAppEventsReverseCommits prova que um fato atrasado não é perdido pelo dispatcher.
func TestAppEventsReverseCommits(t *testing.T) {
	app, ch, user := appEventFixture(t)
	tx, err := GetDB().BeginTx(testCtx(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	first, _, err := createMessageInTx(testCtx(), tx, ch.ID, user.ID, "", "", nil, "", "first", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	second := eventMessage(t, ch, user, "second")
	dispatchEvents(t)
	ds := eventDeliveries(t, app.ID)
	if len(ds) != 1 || *ds[0].MessageID != second.ID {
		t.Fatalf("antes do commit=%+v", ds)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	dispatchEvents(t)
	ds = eventDeliveries(t, app.ID)
	if len(ds) != 2 || *ds[1].MessageID != first.ID || ds[1].DeliverySeq != 2 {
		t.Fatalf("após commit=%+v", ds)
	}
}

// TestAppEventsCrashClaimAndAssignment simula morte nos dois lados da atribuição atômica.
func TestAppEventsCrashClaimAndAssignment(t *testing.T) {
	app, ch, user := appEventFixture(t)
	eventMessage(t, ch, user, "durable")
	for _, assign := range []bool{false, true} {
		tx, err := GetDB().BeginTx(testCtx(), nil)
		if err != nil {
			t.Fatal(err)
		}
		e, err := claimAppEventInTx(testCtx(), tx, nil)
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if assign {
			if err := assignAppEventInTx(testCtx(), tx, e, 100); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
		}
		// Outro dispatcher ignora a linha bloqueada, sem marcar um cursor global.
		if n, err := DispatchAppEvents(testCtx(), 10, 100); err != nil || n != 0 {
			tx.Rollback()
			t.Fatalf("skip locked: %d %v", n, err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	dispatchEvents(t)
	one := eventDeliveries(t, app.ID)
	dispatchEvents(t)
	two := eventDeliveries(t, app.ID)
	if len(one) != 1 || len(two) != 1 || one[0].EventID != two[0].EventID || two[0].DeliverySeq != 1 {
		t.Fatalf("recovery=%+v / %+v", one, two)
	}
}

// TestAppEventsConcurrentDispatch atribui sequências únicas com dois workers concorrentes.
func TestAppEventsConcurrentDispatch(t *testing.T) {
	app, ch, user := appEventFixture(t)
	for i := 0; i < 20; i++ {
		eventMessage(t, ch, user, "concurrent")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := DispatchAppEvents(testCtx(), 100, 100); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	ds := eventDeliveries(t, app.ID)
	if len(ds) != 20 {
		t.Fatalf("entregas=%d", len(ds))
	}
	ids := map[string]bool{}
	for i, d := range ds {
		if ids[d.EventID] || d.DeliverySeq != int64(i+1) {
			t.Fatalf("duplicada ou lacuna: %+v", d)
		}
		ids[d.EventID] = true
	}
}

// TestAppEventsOfflineRedaction cobre snapshots antes e depois da atribuição, sem ack.
func TestAppEventsOfflineRedaction(t *testing.T) {
	for _, assigned := range []bool{false, true} {
		t.Run(map[bool]string{false: "outbox", true: "assigned"}[assigned], func(t *testing.T) {
			app, ch, user := appEventFixture(t)
			m := eventMessage(t, ch, user, "hello")
			if _, err := UpdateMessage(testCtx(), m.ID, models.Message{Content: strPtr("edit")}); err != nil {
				t.Fatal(err)
			}
			if assigned {
				dispatchEvents(t)
			}
			if err := DeleteMessage(testCtx(), m.ID); err != nil {
				t.Fatal(err)
			}
			dispatchEvents(t)
			ds := eventDeliveries(t, app.ID)
			if len(ds) != 3 {
				t.Fatalf("deliveries=%+v", ds)
			}
			for i := 0; i < 2; i++ {
				var payload map[string]any
				if err := json.Unmarshal(ds[i].Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if len(payload) != 3 || payload["redacted"] != true || payload["message_id"] != m.ID {
					t.Fatalf("vazamento: %s", ds[i].Payload)
				}
			}
			if ds[2].Type != "message.deleted" {
				t.Fatalf("delete=%+v", ds[2])
			}
		})
	}
}

// TestAppEventsTombstonesAfterScopeRemoval preserva apenas tombstones para destinatários capturados.
func TestAppEventsTombstonesAfterScopeRemoval(t *testing.T) {
	for _, assigned := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-dispatch", true: "after-dispatch"}[assigned], func(t *testing.T) {
			app, ch, user := appEventFixture(t)
			m := eventMessage(t, ch, user, "secret")
			if err := DeleteMessage(testCtx(), m.ID); err != nil {
				t.Fatal(err)
			}
			if assigned {
				dispatchEvents(t)
			}
			if err := RemoveAppScope(testCtx(), app.ID, ch.ID); err != nil {
				t.Fatal(err)
			}
			dispatchEvents(t)
			ds := eventDeliveries(t, app.ID)
			if len(ds) != 1 || ds[0].Type != "message.deleted" {
				t.Fatalf("scope removido: %+v", ds)
			}
			if err := RevokeApp(testCtx(), app.ID); err != nil {
				t.Fatal(err)
			}
			if ds := eventDeliveries(t, app.ID); len(ds) != 0 {
				t.Fatalf("revogado: %+v", ds)
			}
		})
	}
}

// TestAppEventsChannelDeleteOffline captura recipients antes de apagar os escopos por FK.
func TestAppEventsChannelDeleteOffline(t *testing.T) {
	app, ch, user := appEventFixture(t)
	eventMessage(t, ch, user, "secret")
	if err := DeleteChannel(testCtx(), ch.ID); err != nil {
		t.Fatal(err)
	}
	dispatchEvents(t)
	ds := eventDeliveries(t, app.ID)
	if len(ds) != 1 || ds[0].Type != "channel.deleted" {
		t.Fatalf("canal excluído: %+v", ds)
	}
	var payload map[string]any
	if err := json.Unmarshal(ds[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 1 || payload["channel_id"] != ch.ID {
		t.Fatalf("tombstone: %v", payload)
	}
}

// TestAppEventsNoReaders não grava fatos quando não há apps, leitura ou escopo compatível.
func TestAppEventsNoReaders(t *testing.T) {
	app, ch, user := appEventFixture(t)
	if err := RevokeApp(testCtx(), app.ID); err != nil {
		t.Fatal(err)
	}
	m := eventMessage(t, ch, user, "no reader")
	if _, err := UpdateMessage(testCtx(), m.ID, models.Message{Content: strPtr("edit")}); err != nil {
		t.Fatal(err)
	}
	unicode := "👍"
	if _, _, _, err := AddReaction(testCtx(), m.ID, user.ID, nil, &unicode); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveReaction(testCtx(), m.ID, user.ID, nil, &unicode); err != nil {
		t.Fatal(err)
	}
	if err := DeleteMessage(testCtx(), m.ID); err != nil {
		t.Fatal(err)
	}
	if err := DeleteChannel(testCtx(), ch.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := GetDB().QueryRow("SELECT count(*) FROM app_event_outbox").Scan(&count); err != nil || count != 0 {
		t.Fatalf("outbox sem leitores=%d %v", count, err)
	}
}

// TestAppEventsNewInstallStartsNow exclui o backlog anterior à nova instalação.
func TestAppEventsNewInstallStartsNow(t *testing.T) {
	_, ch, user := appEventFixture(t)
	eventMessage(t, ch, user, "before new install")
	hash := uuid.NewString()
	app, err := CreateApp(testCtx(), "new", "remote", "New", user.ID, models.AppCapabilities{ReadMessages: true}, &hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddAppScope(testCtx(), app.ID, ch.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	eventMessage(t, ch, user, "now")
	dispatchEvents(t)
	ds := eventDeliveries(t, app.ID)
	if len(ds) != 1 {
		t.Fatalf("backfill=%+v", ds)
	}
}

// TestAppEventsRetentionFloor registra perdas por idade e por teto, inclusive antes do dispatch.
func TestAppEventsRetentionFloor(t *testing.T) {
	for _, assigned := range []bool{false, true} {
		t.Run(map[bool]string{false: "undispatched", true: "dispatched"}[assigned], func(t *testing.T) {
			app, ch, user := appEventFixture(t)
			m := eventMessage(t, ch, user, "expire")
			if assigned {
				dispatchEvents(t)
			}
			if _, err := GetDB().Exec("UPDATE app_event_outbox SET created_at=clock_timestamp()-interval '8 days' WHERE message_id=$1", m.ID); err != nil {
				t.Fatal(err)
			}
			// Mantém a elegibilidade histórica: instalação existia antes desse fato.
			if _, err := GetDB().Exec("UPDATE app_installations SET created_at=clock_timestamp()-interval '9 days' WHERE id=$1", app.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := PruneAppEvents(testCtx(), 7*24*time.Hour, 100, 100); err != nil {
				t.Fatal(err)
			}
			var floor, next int64
			if err := GetDB().QueryRow("SELECT replay_floor,next_seq FROM app_delivery_state WHERE installation_id=$1", app.ID).Scan(&floor, &next); err != nil || floor != 1 || next != 1 {
				t.Fatalf("floor=%d next=%d err=%v", floor, next, err)
			}
			if ds := eventDeliveries(t, app.ID); len(ds) != 0 {
				t.Fatalf("expirado=%+v", ds)
			}
		})
	}
	app, ch, user := appEventFixture(t)
	for i := 0; i < 5; i++ {
		eventMessage(t, ch, user, "cap")
	}
	if _, err := DispatchAppEvents(testCtx(), 100, 2); err != nil {
		t.Fatal(err)
	}
	ds := eventDeliveries(t, app.ID)
	if len(ds) != 2 || ds[0].DeliverySeq != 4 || ds[1].DeliverySeq != 5 {
		t.Fatalf("cap=%+v", ds)
	}
	var floor int64
	if err := GetDB().QueryRow("SELECT replay_floor FROM app_delivery_state WHERE installation_id=$1", app.ID).Scan(&floor); err != nil || floor != 3 {
		t.Fatalf("floor=%d %v", floor, err)
	}
	if _, err := PruneAppEvents(testCtx(), 0, 100, 2); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := GetDB().QueryRow("SELECT count(*) FROM app_event_outbox").Scan(&count); err != nil || count != 2 {
		t.Fatalf("snapshot órfão: %d %v", count, err)
	}
}

// TestAppEventsRollback garante que snapshot, redação e recipients não sobrevivem a rollback.
func TestAppEventsRollback(t *testing.T) {
	app, ch, user := appEventFixture(t)
	m := eventMessage(t, ch, user, "keep")
	failTransaction(t, func(tx *sql.Tx) {
		if err := deleteMessageInTx(testCtx(), tx, m.ID); err != nil {
			t.Fatal(err)
		}
	})
	failTransaction(t, func(tx *sql.Tx) {
		if err := deleteChannelInTx(testCtx(), tx, ch.ID); err != nil {
			t.Fatal(err)
		}
	})
	dispatchEvents(t)
	ds := eventDeliveries(t, app.ID)
	if len(ds) != 1 {
		t.Fatalf("rollback=%+v", ds)
	}
	var payload map[string]any
	if err := json.Unmarshal(ds[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["content"] != "keep" {
		t.Fatalf("redação sobreviveu rollback: %s", ds[0].Payload)
	}
}

// TestAppEventsCancellation não deixa SQL bloqueado travar o encerramento do dispatcher.
func TestAppEventsCancellation(t *testing.T) {
	app, ch, user := appEventFixture(t)
	eventMessage(t, ch, user, "cancel")
	if _, err := GetDB().Exec("INSERT INTO app_delivery_state(installation_id) VALUES($1)", app.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := GetDB().BeginTx(testCtx(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("SELECT installation_id FROM app_delivery_state WHERE installation_id=$1 FOR UPDATE", app.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(testCtx(), 100*time.Millisecond)
	defer cancel()
	if _, err := DispatchAppEvents(ctx, 10, 100); err == nil {
		t.Fatal("dispatcher ignorou cancelamento")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// Aguarda o rollback automático de database/sql liberar o lock da claim cancelada.
	var released string
	if err := GetDB().QueryRow("SELECT event_id FROM app_event_outbox WHERE message_id IS NOT NULL LIMIT 1 FOR UPDATE").Scan(&released); err != nil {
		t.Fatal(err)
	}
	dispatchEvents(t)
	if ds := eventDeliveries(t, app.ID); len(ds) != 1 {
		t.Fatalf("cancelamento perdeu evento: %+v", ds)
	}
}

// TestAppEventsAuthorizationDropIsPermanent impede ressuscitar conteúdo após uma entrega negada.
func TestAppEventsAuthorizationDropIsPermanent(t *testing.T) {
	app, ch, user := appEventFixture(t)
	eventMessage(t, ch, user, "secret")
	dispatchEvents(t)
	if err := RemoveAppScope(testCtx(), app.ID, ch.ID); err != nil {
		t.Fatal(err)
	}
	if ds := eventDeliveries(t, app.ID); len(ds) != 0 {
		t.Fatalf("sem escopo: %+v", ds)
	}
	if err := AddAppScope(testCtx(), app.ID, ch.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	if ds := eventDeliveries(t, app.ID); len(ds) != 0 {
		t.Fatalf("conteúdo ressuscitado: %+v", ds)
	}
}

// TestAppEventsMissingSubscription cobre a ausência real de apps e as demais negações da produção.
func TestAppEventsMissingSubscription(t *testing.T) {
	for _, mode := range []string{"no-apps", "no-capability", "no-scope", "dm"} {
		t.Run(mode, func(t *testing.T) {
			app, ch, user := appEventFixture(t)
			switch mode {
			case "no-apps":
				if _, err := GetDB().Exec("DELETE FROM app_installations"); err != nil {
					t.Fatal(err)
				}
			case "no-capability":
				if _, err := UpdateApp(testCtx(), app.ID, nil, nil, nil, &models.AppCapabilities{SendMessages: true}); err != nil {
					t.Fatal(err)
				}
			case "no-scope":
				if err := RemoveAppScope(testCtx(), app.ID, ch.ID); err != nil {
					t.Fatal(err)
				}
			case "dm":
				if _, err := GetDB().Exec("UPDATE channels SET type='dm' WHERE id=$1", ch.ID); err != nil {
					t.Fatal(err)
				}
			}
			eventMessage(t, ch, user, "no events")
			var count int
			if err := GetDB().QueryRow("SELECT count(*) FROM app_event_outbox").Scan(&count); err != nil || count != 0 {
				t.Fatalf("outbox=%d %v", count, err)
			}
		})
	}
}

// TestAppEventsMaterializedRecipients não expande tombstones para leitores adicionados depois.
func TestAppEventsMaterializedRecipients(t *testing.T) {
	app, ch, user := appEventFixture(t)
	m := eventMessage(t, ch, user, "secret")
	if err := DeleteMessage(testCtx(), m.ID); err != nil {
		t.Fatal(err)
	}
	// Perder leitura não cancela o tombstone já endereçado.
	if _, err := UpdateApp(testCtx(), app.ID, nil, nil, nil, &models.AppCapabilities{}); err != nil {
		t.Fatal(err)
	}
	hash := uuid.NewString()
	late, err := CreateApp(testCtx(), "late", "remote", "Late", user.ID, models.AppCapabilities{ReadMessages: true}, &hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddAppScope(testCtx(), late.ID, ch.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	dispatchEvents(t)
	if ds := eventDeliveries(t, app.ID); len(ds) != 1 || ds[0].Type != "message.deleted" {
		t.Fatalf("capacidade removida: %+v", ds)
	}
	if ds := eventDeliveries(t, late.ID); len(ds) != 0 {
		t.Fatalf("destinatário tardio: %+v", ds)
	}
}

// TestAppEventsAssignmentIdempotency protege a unicidade sem consumir uma nova sequência.
func TestAppEventsAssignmentIdempotency(t *testing.T) {
	app, ch, user := appEventFixture(t)
	eventMessage(t, ch, user, "once")
	dispatchEvents(t)
	if _, err := GetDB().Exec("UPDATE app_event_outbox SET dispatched_at=NULL"); err != nil {
		t.Fatal(err)
	}
	dispatchEvents(t)
	ds := eventDeliveries(t, app.ID)
	if len(ds) != 1 || ds[0].DeliverySeq != 1 {
		t.Fatalf("duplicado: %+v", ds)
	}
	var seq int64
	if err := GetDB().QueryRow("SELECT next_seq FROM app_delivery_state WHERE installation_id=$1", app.ID).Scan(&seq); err != nil || seq != 1 {
		t.Fatalf("seq=%d %v", seq, err)
	}
}

// TestAppEventsRedactionWaitsForAssignment impede vazamento quando a redação espera a claim.
func TestAppEventsRedactionWaitsForAssignment(t *testing.T) {
	app, ch, user := appEventFixture(t)
	m := eventMessage(t, ch, user, "must redact")
	assign, err := GetDB().BeginTx(testCtx(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer assign.Rollback()
	e, err := claimAppEventInTx(testCtx(), assign, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := assignAppEventInTx(testCtx(), assign, e, 100); err != nil {
		t.Fatal(err)
	}
	redact, err := GetDB().BeginTx(testCtx(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer redact.Rollback()
	var pid int
	if err := redact.QueryRow("SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		err := redactAppEventsInTx(testCtx(), redact, "message_id", m.ID)
		if err == nil {
			err = redact.Commit()
		}
		done <- err
	}()
	// Espera o bloqueio real no PostgreSQL, sem presumir a ordem do escalonador.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		var waiting bool
		if err := GetDB().QueryRow("SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')", pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("redação não aguardou lock")
		case <-poll.C:
		}
	}
	if err := assign.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-deadline.C:
		t.Fatal("redação bloqueada após commit")
	}
	ds := eventDeliveries(t, app.ID)
	if len(ds) != 1 {
		t.Fatalf("entregas=%+v", ds)
	}
	var payload map[string]any
	if err := json.Unmarshal(ds[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["redacted"] != true || len(payload) != 3 {
		t.Fatalf("vazamento: %s", ds[0].Payload)
	}
}

// TestAppEventsLoweredCap poda backlog existente ao reduzir a configuração, sem novas escritas.
func TestAppEventsLoweredCap(t *testing.T) {
	app, ch, user := appEventFixture(t)
	for i := 0; i < 5; i++ {
		eventMessage(t, ch, user, "existing backlog")
	}
	dispatchEvents(t)
	if _, err := PruneAppEvents(testCtx(), 0, 100, 2); err != nil {
		t.Fatal(err)
	}
	ds := eventDeliveries(t, app.ID)
	if len(ds) != 2 || ds[0].DeliverySeq != 4 {
		t.Fatalf("novo teto ignorado: %+v", ds)
	}
}
