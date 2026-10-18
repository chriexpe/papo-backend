package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var (
	// ErrAppGatewayFenced rejeita conexões substituídas ou instalações revogadas.
	ErrAppGatewayFenced = errors.New("conexão do app substituída ou revogada")
	// ErrAppResyncRequired exige reconciliação REST quando o replay perdeu um prefixo.
	ErrAppResyncRequired = errors.New("reconciliação do app necessária")
	// ErrAppGatewayCursor indica cursor futuro, negativo ou confirmação não enviada.
	ErrAppGatewayCursor = errors.New("cursor de eventos do app inválido")
)

// AppGatewayState separa sequência atribuída, confirmação e piso de retenção.
type AppGatewayState struct {
	Epoch       int64 `json:"epoch"`
	Cursor      int64 `json:"cursor"`
	AckedSeq    int64 `json:"acked_seq"`
	ReplayFloor int64 `json:"replay_floor"`
	LastSeq     int64 `json:"last_seq"`
}

// OpenAppGateway incrementa a época sob o mesmo lock de estado usado pelo dispatcher.
// Um cursor explícito é um checkpoint declarado pelo consumidor, sem confirmar entregas.
// Mesmo resync_required confirma a nova época para cercar a conexão anterior.
func OpenAppGateway(ctx context.Context, id string, resume *int64) (AppGatewayState, error) {
	var s AppGatewayState
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return s, err
	}
	defer tx.Rollback()
	var active string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM app_installations WHERE id=$1 AND revoked_at IS NULL AND kind='remote' FOR SHARE`, id).Scan(&active); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return s, ErrAppGatewayFenced
		}
		return s, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO app_delivery_state(installation_id) VALUES($1) ON CONFLICT DO NOTHING`, id); err != nil {
		return s, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT connection_epoch,acked_seq,replay_floor,next_seq FROM app_delivery_state
  WHERE installation_id=$1 FOR UPDATE`, id).Scan(&s.Epoch, &s.AckedSeq, &s.ReplayFloor, &s.LastSeq); err != nil {
		return s, err
	}
	s.Cursor = s.AckedSeq
	if resume != nil {
		s.Cursor = *resume
	}
	if s.Cursor < 0 || s.Cursor > s.LastSeq {
		return s, ErrAppGatewayCursor
	}
	s.Epoch++
	if _, err := tx.ExecContext(ctx, "UPDATE app_delivery_state SET connection_epoch=$2 WHERE installation_id=$1", id, s.Epoch); err != nil {
		return s, err
	}
	if err := tx.Commit(); err != nil {
		return s, err
	}
	if s.Cursor < s.ReplayFloor {
		return s, ErrAppResyncRequired
	}
	return s, nil
}

// GetAppGatewayState revalida época e atividade sem depender do registro em memória.
func GetAppGatewayState(ctx context.Context, id string, epoch int64) (AppGatewayState, error) {
	var s AppGatewayState
	err := GetDB().QueryRowContext(ctx, `SELECT s.connection_epoch,s.acked_seq,s.replay_floor,s.next_seq
  FROM app_delivery_state s JOIN app_installations a ON a.id=s.installation_id
  WHERE s.installation_id=$1 AND a.revoked_at IS NULL AND s.connection_epoch=$2`, id, epoch).Scan(&s.Epoch, &s.AckedSeq, &s.ReplayFloor, &s.LastSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrAppGatewayFenced
	}
	return s, err
}

// PendingAppGatewaySequences enfileira apenas IDs, sem reter snapshots que podem ser redigidos.
func PendingAppGatewaySequences(ctx context.Context, id string, epoch, after int64, limit int) ([]int64, AppGatewayState, error) {
	s, err := GetAppGatewayState(ctx, id, epoch)
	s.Cursor = after
	if err != nil {
		return nil, s, err
	}
	if after < s.ReplayFloor {
		return nil, s, ErrAppResyncRequired
	}
	if limit <= 0 || limit > 100 {
		limit = 16
	}
	rows, err := GetDB().QueryContext(ctx, `SELECT delivery_seq FROM app_event_deliveries
  WHERE installation_id=$1 AND delivery_seq>$2 ORDER BY delivery_seq LIMIT $3`, id, after, limit)
	if err != nil {
		return nil, s, err
	}
	defer rows.Close()
	seqs := make([]int64, 0)
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, s, err
		}
		seqs = append(seqs, seq)
	}
	return seqs, s, rows.Err()
}

// ReadAppGatewayDelivery consulta o snapshot e a autorização imediatamente antes do envio.
// Ausência por perda de escopo é descarte; ausência por retenção exige resync.
func ReadAppGatewayDelivery(ctx context.Context, id string, epoch, seq int64) (*AppEventDelivery, AppGatewayState, error) {
	s, err := GetAppGatewayState(ctx, id, epoch)
	s.Cursor = seq - 1
	if err != nil {
		return nil, s, err
	}
	if seq <= s.AckedSeq {
		return nil, s, nil
	}
	if seq <= s.ReplayFloor {
		return nil, s, ErrAppResyncRequired
	}
	ds, err := ListAppEventDeliveries(ctx, id, seq-1, 1)
	if err != nil {
		return nil, s, err
	}
	if len(ds) == 0 || ds[0].DeliverySeq != seq {
		// A poda pode ter confirmado entre a leitura do estado e a busca do payload.
		// Recarrega o piso para não confundir expiração concorrente com descarte por ACL.
		s, err = GetAppGatewayState(ctx, id, epoch)
		s.Cursor = seq - 1
		if err == nil && seq <= s.ReplayFloor && seq > s.AckedSeq {
			return nil, s, ErrAppResyncRequired
		}
		return nil, s, err
	}
	return &ds[0], s, nil
}

// AckAppGateway confirma cumulativamente somente o prefixo enviado na época corrente.
// Remover entregas libera snapshots compartilhados para a poda quando o último leitor confirmar.
func AckAppGateway(ctx context.Context, id string, epoch, seq, highestSent int64) error {
	if seq < 0 || seq > highestSent {
		return ErrAppGatewayCursor
	}
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE app_delivery_state s SET acked_seq=GREATEST(acked_seq,$3),replay_floor=GREATEST(replay_floor,$3)
  WHERE installation_id=$1 AND connection_epoch=$2 AND next_seq >= $3
  AND EXISTS(SELECT 1 FROM app_installations a WHERE a.id=s.installation_id AND a.revoked_at IS NULL)`, id, epoch, seq)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrAppGatewayFenced
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM app_event_deliveries WHERE installation_id=$1 AND delivery_seq<=$2", id, seq); err != nil {
		return fmt.Errorf("falha ao confirmar eventos: %w", err)
	}
	return tx.Commit()
}
