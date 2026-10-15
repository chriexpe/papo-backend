package storage

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestAppGatewayEpochAndAck cerca confirmações antigas e não aceita o prefixo ainda não enviado.
func TestAppGatewayEpochAndAck(t *testing.T) {
	app, ch, user := appEventFixture(t)
	eventMessage(t, ch, user, "one")
	dispatchEvents(t)
	first, err := OpenAppGateway(testCtx(), app.ID, nil)
	if err != nil || first.Epoch != 1 {
		t.Fatalf("primeira=%+v %v", first, err)
	}
	second, err := OpenAppGateway(testCtx(), app.ID, nil)
	if err != nil || second.Epoch != 2 {
		t.Fatalf("segunda=%+v %v", second, err)
	}
	if err := AckAppGateway(testCtx(), app.ID, first.Epoch, 1, 1); !errors.Is(err, ErrAppGatewayFenced) {
		t.Fatalf("ack antigo=%v", err)
	}
	if err := AckAppGateway(testCtx(), app.ID, second.Epoch, 1, 0); !errors.Is(err, ErrAppGatewayCursor) {
		t.Fatalf("ack sem envio=%v", err)
	}
	if err := AckAppGateway(testCtx(), app.ID, second.Epoch, 1, 1); err != nil {
		t.Fatal(err)
	}
	// A confirmação é durável e repetível, sem depender do registro em memória.
	if err := AckAppGateway(testCtx(), app.ID, second.Epoch, 1, 1); err != nil {
		t.Fatal(err)
	}
	state, err := GetAppGatewayState(testCtx(), app.ID, second.Epoch)
	if err != nil || state.AckedSeq != 1 || state.ReplayFloor != 1 {
		t.Fatalf("estado=%+v %v", state, err)
	}
	if _, err := PruneAppEvents(testCtx(), 0, 100, 100); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := GetDB().QueryRow("SELECT count(*) FROM app_event_outbox").Scan(&count); err != nil || count != 0 {
		t.Fatalf("snapshot confirmado=%d %v", count, err)
	}
}

// TestAppGatewayConcurrentEpochs valida dois upgrades concorrentes sem reutilização de época.
func TestAppGatewayConcurrentEpochs(t *testing.T) {
	app, _, _ := appEventFixture(t)
	var wg sync.WaitGroup
	states := make(chan AppGatewayState, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state, err := OpenAppGateway(testCtx(), app.ID, nil)
			states <- state
			errs <- err
		}()
	}
	wg.Wait()
	close(states)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[int64]bool{}
	for state := range states {
		seen[state.Epoch] = true
	}
	if !seen[1] || !seen[2] {
		t.Fatalf("épocas=%v", seen)
	}
	if err := AckAppGateway(testCtx(), app.ID, 1, 0, 0); !errors.Is(err, ErrAppGatewayFenced) {
		t.Fatal(err)
	}
}

// TestAppGatewayReadsFreshSnapshots não mantém conteúdo enfileirado antes de uma exclusão.
func TestAppGatewayReadsFreshSnapshots(t *testing.T) {
	app, ch, user := appEventFixture(t)
	m := eventMessage(t, ch, user, "old content")
	dispatchEvents(t)
	s, err := OpenAppGateway(testCtx(), app.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	seqs, _, err := PendingAppGatewaySequences(testCtx(), app.ID, s.Epoch, 0, 16)
	if err != nil || len(seqs) != 1 {
		t.Fatalf("seqs=%v %v", seqs, err)
	}
	if err := DeleteMessage(testCtx(), m.ID); err != nil {
		t.Fatal(err)
	}
	d, _, err := ReadAppGatewayDelivery(testCtx(), app.ID, s.Epoch, seqs[0])
	if err != nil || d == nil || string(d.Payload) == "" {
		t.Fatalf("evento=%+v %v", d, err)
	}
	var redacted bool
	if err := GetDB().QueryRow("SELECT (payload->>'redacted')::boolean FROM app_event_outbox WHERE event_id=$1", d.EventID).Scan(&redacted); err != nil || !redacted {
		t.Fatalf("redação=%v %v", redacted, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(d.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 3 || payload["redacted"] != true {
		t.Fatalf("conteúdo vazou: %s", d.Payload)
	}

}

// TestAppGatewayRetentionAndCursor rejeita replay expirado, cursores futuros e épocas obsoletas.
func TestAppGatewayRetentionAndCursor(t *testing.T) {
	app, ch, user := appEventFixture(t)
	eventMessage(t, ch, user, "expire")
	dispatchEvents(t)
	state, err := OpenAppGateway(testCtx(), app.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GetDB().Exec("UPDATE app_event_outbox SET created_at=clock_timestamp()-interval '8 days'"); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneAppEvents(testCtx(), 0, 100, 100); err != nil {
		t.Fatal(err)
	}
	if _, _, err := PendingAppGatewaySequences(testCtx(), app.ID, state.Epoch, 0, 16); !errors.Is(err, ErrAppResyncRequired) {
		t.Fatalf("replay=%v", err)
	}
	if _, _, err := ReadAppGatewayDelivery(testCtx(), app.ID, state.Epoch, 1); !errors.Is(err, ErrAppResyncRequired) {
		t.Fatalf("leitura=%v", err)
	}
	zero := int64(0)
	expired, err := OpenAppGateway(testCtx(), app.ID, &zero)
	if !errors.Is(err, ErrAppResyncRequired) || expired.ReplayFloor != 1 {
		t.Fatalf("expirado=%+v %v", expired, err)
	}
	for _, cursor := range []int64{-1, 2} {
		if _, err := OpenAppGateway(testCtx(), app.ID, &cursor); !errors.Is(err, ErrAppGatewayCursor) {
			t.Fatalf("cursor=%d %v", cursor, err)
		}
	}
	if err := RevokeApp(testCtx(), app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAppGateway(testCtx(), app.ID, nil); !errors.Is(err, ErrAppGatewayFenced) {
		t.Fatalf("revogado=%v", err)
	}
}

// TestAppGatewayConcurrentPruning não confunde uma expiração durante o fetch com descarte por ACL.
func TestAppGatewayConcurrentPruning(t *testing.T) {
	app, ch, user := appEventFixture(t)
	m := eventMessage(t, ch, user, "race expiry")
	dispatchEvents(t)
	state, err := OpenAppGateway(testCtx(), app.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveAppScope(testCtx(), app.ID, ch.ID); err != nil {
		t.Fatal(err)
	}
	holder, err := GetDB().BeginTx(testCtx(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	var eventID string
	if err := holder.QueryRow("SELECT event_id FROM app_event_deliveries WHERE installation_id=$1 FOR UPDATE", app.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(testCtx(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, _, err := ReadAppGatewayDelivery(ctx, app.ID, state.Epoch, 1); result <- err }()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		var waiting bool
		if err := GetDB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
   WHERE datname=current_database() AND wait_event_type='Lock'
    AND query LIKE 'DELETE FROM app_event_deliveries d USING%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("fetch de %s não aguardou lock", m.ID)
		case <-poll.C:
		}
	}
	if err := expireAppEventInTx(ctx, holder, eventID); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrAppResyncRequired) {
			t.Fatalf("expiração concorrente=%v", err)
		}
	case <-ctx.Done():
		t.Fatal("fetch não encerrou")
	}
}
