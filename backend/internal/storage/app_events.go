package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"papo/internal/models"
)

// AppEvent é um fato persistido; EventID é a chave de deduplicação, nunca um cursor.
type AppEvent struct {
	EventID   string          `json:"event_id"`
	Type      string          `json:"type"`
	ChannelID string          `json:"channel_id"`
	MessageID *string         `json:"message_id,omitempty"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// appEventsWritten acorda o dispatcher depois do commit de uma escrita que pode
// ter gravado fatos no outbox. O backend roda em um único processo, então o
// sinal é em memória; o dispatcher mantém uma varredura lenta de segurança.
var appEventsWritten = make(chan struct{}, 1)

// notifyAppEvents sinaliza sem bloquear: sinais acumulados viram um só, pois
// o dispatcher drena o outbox inteiro a cada despertar.
func notifyAppEvents() {
	select {
	case appEventsWritten <- struct{}{}:
	default:
	}
}

// AppEventsWritten devolve o canal sinalizado após o commit de mutações que
// produzem eventos de apps. Deve ser consumido por um único dispatcher.
func AppEventsWritten() <-chan struct{} {
	return appEventsWritten
}

// appEventReader limita a produção a instalações ativas com leitura e ACL de máquina.
const appEventReader = `FROM app_installations a
 JOIN app_channel_scopes s ON s.app_installation_id=a.id
 JOIN channels c ON c.id=s.channel_id
 WHERE s.channel_id=$1 AND a.revoked_at IS NULL
 AND a.capabilities @> '{"read_messages":true}'::jsonb AND c.type IN ('text','voice')`

// createAppEventInTx grava somente quando há um leitor, na transação da mutação.
// Tombstones materializam os destinatários antes de qualquer cascata de escopos.
func createAppEventInTx(ctx context.Context, tx *sql.Tx, eventType, channelID string, messageID *string, payload any) error {
	var interested bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 "+appEventReader+")", channelID).Scan(&interested); err != nil {
		return fmt.Errorf("falha ao consultar leitores de eventos: %w", err)
	}
	if !interested {
		return nil
	}
	// Os anexos já estão vinculados, mas embeds/previews ainda não fazem parte do fato.
	if eventType == "message.created" {
		snapshot, ok := payload.(map[string]any)
		if !ok || messageID == nil {
			return fmt.Errorf("snapshot de mensagem inválido")
		}
		rows, err := tx.QueryContext(ctx, `SELECT a.id, m.mime_type, a.original_file_name, m.size_bytes, a.created_at
   FROM attachments a JOIN media m ON m.sha_hash=a.media_sha_hash
   WHERE a.messages_id=$1 ORDER BY a.created_at,a.id`, *messageID)
		if err != nil {
			return err
		}
		attachments := make([]map[string]any, 0)
		for rows.Next() {
			var id, mime, name string
			var size int64
			var created time.Time
			if err := rows.Scan(&id, &mime, &name, &size, &created); err != nil {
				rows.Close()
				return err
			}
			attachments = append(attachments, map[string]any{"id": id, "mime_type": mime, "original_file_name": name, "size_bytes": size, "created_at": created})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		snapshot["attachments"] = attachments
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// O ARRAY é calculado no mesmo statement que o INSERT e sobrevive ao DELETE do canal.
	recipients := "NULL::uuid[]"
	if eventType == "message.deleted" || eventType == "channel.deleted" {
		recipients = "ARRAY(SELECT a.id " + appEventReader + " ORDER BY a.id)"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO app_event_outbox(event_type,channel_id,message_id,payload,recipients)
  SELECT $2,$1,$3,$4,`+recipients, channelID, eventType, messageID, string(data))
	return err
}

// createMessageEventInTx captura a autoria e o conteúdo sem campos derivados de embeds.
func createMessageEventInTx(ctx context.Context, tx *sql.Tx, eventType string, m models.Message) error {
	payload := map[string]any{"message_id": m.ID, "channel_id": m.ChannelID, "content": m.Content}
	if eventType == "message.created" {
		kind, id := "human", m.AuthorID
		if m.WebhookID != nil {
			kind, id = "webhook", m.WebhookID
		}
		if m.AppInstallationID != nil {
			kind, id = "app", m.AppInstallationID
		}
		payload["sender_kind"], payload["sender_id"] = kind, id
		payload["external_author"], payload["reply_to"], payload["created_at"] = m.ExternalAuthor, m.ReplyTo, m.CreatedAt
	} else {
		payload["edited_at"] = m.EditedAt
	}
	return createAppEventInTx(ctx, tx, eventType, m.ChannelID, &m.ID, payload)
}

// redactAppEventsInTx remove conteúdo de snapshots ainda pendentes, inclusive já atribuídos.
// A confirmação é a fronteira durável de entrega; envios sem ack continuam pendentes.
func redactAppEventsInTx(ctx context.Context, tx *sql.Tx, column, id string) error {
	// column vem exclusivamente dos dois pontos de exclusão internos.
	if column != "message_id" && column != "channel_id" {
		return fmt.Errorf("filtro de redação inválido")
	}
	// Primeiro adquire os locks; o UPDATE seguinte ganha um snapshot READ COMMITTED novo.
	// Sem isso, esperar um dispatcher e reavaliar EXISTS no snapshot antigo pode ocultar
	// as entregas que ele acabou de confirmar, deixando conteúdo sem redação.
	rows, err := tx.QueryContext(ctx, "SELECT event_id FROM app_event_outbox WHERE "+column+"=$1 AND event_type IN ('message.created','message.updated') ORDER BY event_id FOR UPDATE", id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var locked string
		if err := rows.Scan(&locked); err != nil {
			rows.Close()
			return err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE app_event_outbox e
  SET payload=jsonb_build_object('message_id',message_id,'channel_id',channel_id,'redacted',true)
  WHERE `+column+`=$1 AND event_type IN ('message.created','message.updated')
  AND (dispatched_at IS NULL OR EXISTS(SELECT 1 FROM app_event_deliveries d WHERE d.event_id=e.event_id))`, id)
	return err
}

// createReactionEventInTx lê o canal na mesma transação, sem incluir conteúdo da mensagem.
func createReactionEventInTx(ctx context.Context, tx *sql.Tx, eventType, messageID, userID string, emojiID, unicode *string) error {
	var channel string
	if err := tx.QueryRowContext(ctx, "SELECT channel_id FROM messages WHERE id=$1", messageID).Scan(&channel); err != nil {
		return mapStorageError(err)
	}
	return createAppEventInTx(ctx, tx, eventType, channel, &messageID, map[string]any{
		"channel_id": channel, "message_id": messageID, "user_id": userID, "emoji_id": emojiID, "unicode": unicode,
	})
}
