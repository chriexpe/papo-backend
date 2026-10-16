package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"papo/internal/appevents"
	"papo/internal/models"
	"papo/internal/storage"
)

// TestAppEventWorkerDrainsExpiredBacklog exercita o worker real, além da API de poda.
// Um lote cheio precisa provocar continuação imediata para não limitar a poda a 200/min.
func TestAppEventWorkerDrainsExpiredBacklog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db := storage.GetDB()
	var user string
	if err := db.QueryRowContext(ctx, "INSERT INTO users(username,password_hash) VALUES($1,'test') RETURNING id", uuid.NewString()).Scan(&user); err != nil {
		t.Fatal(err)
	}
	ch, err := storage.CreateChannel(ctx, uuid.NewString(), "text", "")
	if err != nil {
		t.Fatal(err)
	}
	hash := uuid.NewString()
	app, err := storage.CreateApp(ctx, "worker-backlog", "remote", "Reader", user, models.AppCapabilities{ReadMessages: true}, &hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.AddAppScope(ctx, app.ID, ch.ID, user); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE app_installations SET created_at=clock_timestamp()-interval '9 days' WHERE id=$1", app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO app_event_outbox(event_type,channel_id,payload,created_at)
  SELECT 'message.created',$1,'{}'::jsonb,clock_timestamp()-interval '8 days' FROM generate_series(1,600)`, ch.ID); err != nil {
		t.Fatal(err)
	}
	worker := appevents.Start(ctx, 7*24*time.Hour, 1000)
	defer worker.Shutdown()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM app_event_outbox WHERE channel_id=$1", ch.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("worker não drenou backlog expirado")
		case <-poll.C:
		}
	}
	var floor, next int64
	if err := db.QueryRowContext(ctx, "SELECT replay_floor,next_seq FROM app_delivery_state WHERE installation_id=$1", app.ID).Scan(&floor, &next); err != nil || floor != 600 || next != 600 {
		t.Fatalf("floor=%d next=%d err=%v", floor, next, err)
	}
}
