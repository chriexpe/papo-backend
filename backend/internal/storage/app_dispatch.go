package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// DefaultAppEventRetention limita a persistência de snapshots de conteúdo.
const DefaultAppEventRetention = 7 * 24 * time.Hour

// DefaultAppDeliveryCap limita o backlog de cada instalação independentemente da idade.
const DefaultAppDeliveryCap = 10000

const appEventColumns = "event_id,event_type,channel_id,message_id,payload,created_at"

// claimedAppEvent mantém a reserva até a atribuição e o commit, sem lease em memória.
type claimedAppEvent struct {
	AppEvent
	recipients []string
	dispatched bool
}

// claimAppEventInTx reserva um fato confirmado; nenhum cursor usa a ordem dos UUIDs.
func claimAppEventInTx(ctx context.Context, tx *sql.Tx, expiredBefore *time.Time) (claimedAppEvent, error) {
	where := "dispatched_at IS NULL"
	args := []any{}
	if expiredBefore != nil {
		where = `created_at < $1 OR (dispatched_at IS NOT NULL AND NOT EXISTS
   (SELECT 1 FROM app_event_deliveries d WHERE d.event_id=app_event_outbox.event_id))`
		args = append(args, *expiredBefore)
	}
	var e claimedAppEvent
	var recipients []byte
	err := tx.QueryRowContext(ctx, "SELECT "+appEventColumns+`,to_json(recipients),dispatched_at IS NOT NULL
  FROM app_event_outbox WHERE `+where+` ORDER BY created_at,event_id LIMIT 1 FOR UPDATE SKIP LOCKED`, args...).Scan(
		&e.EventID, &e.Type, &e.ChannelID, &e.MessageID, &e.Payload, &e.CreatedAt, &recipients, &e.dispatched)
	if err != nil {
		return e, err
	}
	if recipients != nil {
		err = json.Unmarshal(recipients, &e.recipients)
	}
	return e, err
}

// assignAppEventInTx atribui sequências sob os locks de estado em ordem de instalação.
// Claim, estado e entregas confirmam juntos; um crash antes do commit libera tudo.
func assignAppEventInTx(ctx context.Context, tx *sql.Tx, e claimedAppEvent, cap int) error {
	var rows *sql.Rows
	var err error
	if e.recipients != nil {
		rows, err = tx.QueryContext(ctx, `SELECT id FROM app_installations
   WHERE id=ANY($1) AND revoked_at IS NULL ORDER BY id FOR SHARE`, e.recipients)
	} else {
		rows, err = tx.QueryContext(ctx, "SELECT a.id "+appEventReader+" AND a.created_at <= $2 ORDER BY a.id FOR SHARE OF a", e.ChannelID, e.CreatedAt)
	}
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `INSERT INTO app_delivery_state(installation_id) VALUES($1) ON CONFLICT DO NOTHING`, id); err != nil {
			return err
		}
		var seq int64
		if err = tx.QueryRowContext(ctx, "SELECT next_seq FROM app_delivery_state WHERE installation_id=$1 FOR UPDATE", id).Scan(&seq); err != nil {
			return err
		}
		// A unicidade também protege reprocessamentos administrativos do mesmo fato.
		var existing bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM app_event_deliveries WHERE installation_id=$1 AND event_id=$2)", id, e.EventID).Scan(&existing); err != nil {
			return err
		}
		if existing {
			continue
		}
		seq++
		if _, err = tx.ExecContext(ctx, "INSERT INTO app_event_deliveries(installation_id,delivery_seq,event_id) VALUES($1,$2,$3)", id, seq, e.EventID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE app_delivery_state SET next_seq=$2 WHERE installation_id=$1", id, seq); err != nil {
			return err
		}
		if err = trimAppDeliveriesInTx(ctx, tx, id, cap); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE app_event_outbox SET dispatched_at=clock_timestamp() WHERE event_id=$1", e.EventID)
	return err
}

// trimAppDeliveriesInTx remove o prefixo excedente e avança o piso sem reutilizar sequências.
func trimAppDeliveriesInTx(ctx context.Context, tx *sql.Tx, id string, cap int) error {
	_, err := tx.ExecContext(ctx, `WITH removed AS (
  DELETE FROM app_event_deliveries WHERE installation_id=$1 AND delivery_seq <= COALESCE(
   (SELECT delivery_seq FROM app_event_deliveries WHERE installation_id=$1
    ORDER BY delivery_seq DESC OFFSET $2 LIMIT 1),0) RETURNING delivery_seq
 ) UPDATE app_delivery_state SET replay_floor=GREATEST(replay_floor,
  COALESCE((SELECT MAX(delivery_seq) FROM removed),0)) WHERE installation_id=$1`, id, cap)
	return err
}

// DispatchAppEvents distribui um lote limitado de fatos, uma transação por claim.
// A ordem observável é a atribuição por instalação, não a criação nem o início da transação.
func DispatchAppEvents(ctx context.Context, limit, cap int) (int, error) {
	if limit <= 0 {
		limit = 50
	}
	if cap <= 0 {
		cap = DefaultAppDeliveryCap
	}
	count := 0
	for count < limit {
		tx, err := GetDB().BeginTx(ctx, nil)
		if err != nil {
			return count, err
		}
		e, err := claimAppEventInTx(ctx, tx, nil)
		if errors.Is(err, sql.ErrNoRows) {
			tx.Rollback()
			return count, nil
		}
		if err == nil {
			err = assignAppEventInTx(ctx, tx, e, cap)
		}
		if err == nil {
			err = tx.Commit()
		}
		tx.Rollback()
		if err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// PruneAppEvents expira snapshots, inclusive claims ainda não atribuídos, em lotes.
// Antes de apagar um fato expirado, registra a lacuna nos destinatários elegíveis.
// O piso permite responder resync_required em vez de perder dados silenciosamente.
func PruneAppEvents(ctx context.Context, retention time.Duration, limit, cap int) (int, error) {
	if retention <= 0 {
		retention = DefaultAppEventRetention
	}
	if limit <= 0 {
		limit = 50
	}
	if cap <= 0 {
		cap = DefaultAppDeliveryCap
	}
	// Aplica também um teto reduzido na configuração, mesmo sem fatos novos.
	if err := pruneAppDeliveryCaps(ctx, cap, limit); err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-retention)
	count := 0
	for count < limit {
		tx, err := GetDB().BeginTx(ctx, nil)
		if err != nil {
			return count, err
		}
		e, err := claimAppEventInTx(ctx, tx, &cutoff)
		if errors.Is(err, sql.ErrNoRows) {
			tx.Rollback()
			return count, nil
		}
		if err == nil && !e.dispatched {
			err = assignAppEventInTx(ctx, tx, e, cap)
		}
		if err == nil {
			err = expireAppEventInTx(ctx, tx, e.EventID)
		}
		if err == nil {
			err = tx.Commit()
		}
		tx.Rollback()
		if err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// pruneAppDeliveryCaps ajusta backlogs existentes sem adquirir locks de fatos sob locks de estado.
func pruneAppDeliveryCaps(ctx context.Context, cap, limit int) error {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT installation_id FROM app_delivery_state s
  WHERE EXISTS(SELECT 1 FROM app_event_deliveries d WHERE d.installation_id=s.installation_id
   ORDER BY delivery_seq DESC OFFSET $1 LIMIT 1)
  ORDER BY installation_id LIMIT $2 FOR UPDATE SKIP LOCKED`, cap, limit)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := trimAppDeliveriesInTx(ctx, tx, id, cap); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// expireAppEventInTx segue a mesma ordem de locks do dispatcher: fato, estados ordenados.
func expireAppEventInTx(ctx context.Context, tx *sql.Tx, eventID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT installation_id,delivery_seq FROM app_event_deliveries
  WHERE event_id=$1 ORDER BY installation_id`, eventID)
	if err != nil {
		return err
	}
	type delivery struct {
		id  string
		seq int64
	}
	deliveries := []delivery{}
	for rows.Next() {
		var d delivery
		if err := rows.Scan(&d.id, &d.seq); err != nil {
			rows.Close()
			return err
		}
		deliveries = append(deliveries, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, d := range deliveries {
		if _, err := tx.ExecContext(ctx, `UPDATE app_delivery_state SET replay_floor=GREATEST(replay_floor,$2)
   WHERE installation_id=$1`, d.id, d.seq); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM app_event_outbox WHERE event_id=$1", eventID)
	return err
}

// AppEventDelivery associa o fato à sequência local da instalação.
type AppEventDelivery struct {
	AppEvent
	DeliverySeq int64 `json:"delivery_seq"`
}

// ListAppEventDeliveries revalida leitura e escopo no momento da consulta.
// A leitura de cada entrega repete essa consulta imediatamente antes do envio, sem cache de payloads.
// Tombstones exigem somente instalação ativa; conteúdo perde autorização sem escopo.
func ListAppEventDeliveries(ctx context.Context, installationID string, after int64, limit int) ([]AppEventDelivery, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Descarte durável: readquirir escopo depois não ressuscita fatos já negados.
	if _, err := tx.ExecContext(ctx, `DELETE FROM app_event_deliveries d USING app_event_outbox e, app_installations a
  WHERE d.event_id=e.event_id AND a.id=d.installation_id AND a.id=$1
  AND (a.revoked_at IS NOT NULL OR (e.recipients IS NULL AND
   (NOT (a.capabilities @> '{"read_messages":true}'::jsonb) OR NOT EXISTS(
    SELECT 1 FROM app_channel_scopes s JOIN channels c ON c.id=s.channel_id
    WHERE s.app_installation_id=a.id AND s.channel_id=e.channel_id AND c.type IN ('text','voice')))))`, installationID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.event_id,e.event_type,e.channel_id,e.message_id,e.payload,e.created_at,d.delivery_seq
  FROM app_event_deliveries d JOIN app_event_outbox e ON e.event_id=d.event_id
  JOIN app_installations a ON a.id=d.installation_id
  WHERE d.installation_id=$1 AND d.delivery_seq>$2 AND a.revoked_at IS NULL
  AND (e.recipients IS NOT NULL OR (a.capabilities @> '{"read_messages":true}'::jsonb
   AND EXISTS(SELECT 1 FROM app_channel_scopes s JOIN channels c ON c.id=s.channel_id
    WHERE s.app_installation_id=a.id AND s.channel_id=e.channel_id AND c.type IN ('text','voice'))))
  ORDER BY d.delivery_seq LIMIT $3`, installationID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AppEventDelivery, 0)
	for rows.Next() {
		var d AppEventDelivery
		if err := rows.Scan(&d.EventID, &d.Type, &d.ChannelID, &d.MessageID, &d.Payload, &d.CreatedAt, &d.DeliverySeq); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
