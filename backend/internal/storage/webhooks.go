package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"papo/internal/models"
)

// webhookColumns são as colunas de webhooks expostas no modelo (secret_hash
// fica fora: o hash jamais sai da camada storage).
const webhookColumns = "id, channel_id, name, display_name, avatar_media, created_by, created_at, revoked_at"

// scanWebhook lê uma row de webhooks na ordem de webhookColumns.
func scanWebhook(row rowScanner) (models.Webhook, error) {
	var w models.Webhook
	err := row.Scan(&w.ID, &w.ChannelID, &w.Name, &w.DisplayName, &w.AvatarMedia,
		&w.CreatedBy, &w.CreatedAt, &w.RevokedAt)
	if err != nil {
		return models.Webhook{}, mapStorageError(err)
	}
	return w, nil
}

// MaxActiveWebhooksPerChannel limita a 50 os webhooks ativos de cada canal.
const MaxActiveWebhooksPerChannel = 50

// ErrWebhookLimitReached indica que o canal já atingiu o teto de webhooks ativos.
var ErrWebhookLimitReached = errors.New("limite de 50 webhooks ativos por canal atingido")

// CreateWebhook insere um webhook e persiste somente o hash do token.
// O bloqueio do canal serializa contagem e inserção, inclusive entre processos.
func CreateWebhook(ctx context.Context, channelID, name, creator, secretHash string) (models.Webhook, error) {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return models.Webhook{}, err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, "SELECT id FROM channels WHERE id = $1 FOR UPDATE", channelID).Scan(&id); err != nil {
		return models.Webhook{}, mapStorageError(err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM webhooks WHERE channel_id = $1 AND revoked_at IS NULL", channelID).Scan(&count); err != nil {
		return models.Webhook{}, err
	}
	if count >= MaxActiveWebhooksPerChannel {
		return models.Webhook{}, ErrWebhookLimitReached
	}
	webhook, err := scanWebhook(tx.QueryRowContext(ctx,
		"INSERT INTO webhooks (channel_id, name, created_by, secret_hash) VALUES ($1,$2,$3,$4) RETURNING "+webhookColumns,
		channelID, name, creator, secretHash))
	if err != nil {
		return models.Webhook{}, err
	}
	if err := tx.Commit(); err != nil {
		return models.Webhook{}, err
	}
	return webhook, nil
}

// ListWebhooks aplica a ACL de leitura (como SearchMessages) aos webhooks, antes
// de aplicar o limite. O cursor composto evita perdas em timestamps iguais.
func ListWebhooks(ctx context.Context, actorID string, since *time.Time, lastID string, orderAsc bool, limit int) ([]models.Webhook, error) {
	query := "SELECT " + webhookColumns + ` FROM webhooks WHERE revoked_at IS NULL AND channel_id IN (
 SELECT c.id FROM channels c WHERE c.type <> 'dm' AND (
  EXISTS (SELECT 1 FROM servers s WHERE s.owner_id = $1)
  OR c.permissions IS NULL OR c.permissions = '{}'::jsonb
  OR EXISTS (SELECT 1 FROM user_roles ur JOIN roles r ON r.id = ur.role_id
   WHERE ur.user_id = $1 AND (c.permissions -> r.id::text ->> 'read_channel') = 'true')
 ))`
	args := []any{actorID}
	op, order := "<", "DESC"
	if orderAsc {
		op, order = ">", "ASC"
	}
	if since != nil {
		args = append(args, *since)
		if lastID != "" {
			query += " AND (created_at, id) " + op + " ($2, $3::uuid)"
			args = append(args, lastID)
		} else {
			// since sem last_id preserva o polling de mensagens, em qualquer ordem.
			query += " AND created_at > $2"
		}
	}
	query += " ORDER BY created_at " + order + ", id " + order
	args = append(args, limit)
	query += fmt.Sprintf(" LIMIT $%d", len(args))
	rows, err := GetDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]models.Webhook, 0)
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetWebhookByID busca um webhook pelo id, incluindo revogados (a gestão e a
// atribuição de mensagens existentes precisam enxergar webhooks revogados).
func GetWebhookByID(ctx context.Context, id string) (models.Webhook, error) {
	return scanWebhook(GetDB().QueryRowContext(ctx,
		"SELECT "+webhookColumns+" FROM webhooks WHERE id = $1", id))
}

// GetActiveWebhookWithHash busca um webhook não revogado junto do seu
// secret_hash, para a comparação constant-time do token de acesso.
func GetActiveWebhookWithHash(ctx context.Context, id string) (models.Webhook, string, error) {
	var hash string
	var w models.Webhook
	err := GetDB().QueryRowContext(ctx,
		"SELECT "+webhookColumns+", secret_hash FROM webhooks WHERE id=$1 AND revoked_at IS NULL", id).
		Scan(&w.ID, &w.ChannelID, &w.Name, &w.DisplayName, &w.AvatarMedia,
			&w.CreatedBy, &w.CreatedAt, &w.RevokedAt, &hash)
	if err != nil {
		return models.Webhook{}, "", mapStorageError(err)
	}
	return w, hash, nil
}

// UpdateWebhookIdentity atualiza somente os campos informados da identidade
// do webhook (nickname e/ou referência de avatar). displayName/avatarMedia
// nil preservam o valor atual; sql.NullString{Valid:false} grava NULL (remove
// o nickname — volta a usar Name — ou remove o avatar); sql.NullString com
// Valid:true grava o valor. Retorna o webhook atualizado ou ErrNotFound
// quando o webhook não existe.
func UpdateWebhookIdentity(ctx context.Context, id string, displayName, avatarMedia any) (models.Webhook, error) {
	sets := []string{}
	args := []any{id}
	if displayName != nil {
		args = append(args, displayName)
		sets = append(sets, fmt.Sprintf("display_name = $%d", len(args)))
	}
	if avatarMedia != nil {
		args = append(args, avatarMedia)
		sets = append(sets, fmt.Sprintf("avatar_media = $%d", len(args)))
	}
	if len(sets) == 0 {
		return GetWebhookByID(ctx, id)
	}

	return scanWebhook(GetDB().QueryRowContext(ctx,
		"UPDATE webhooks SET "+strings.Join(sets, ", ")+" WHERE id = $1 RETURNING "+webhookColumns,
		args...))
}

// GetWebhookAvatarMedia resolve a referência media do avatar persistente do
// webhook (NULL quando não há avatar). ErrNotFound quando o webhook não existe.
func GetWebhookAvatarMedia(ctx context.Context, id string) (*string, error) {
	var avatar *string
	err := GetDB().QueryRowContext(ctx, "SELECT avatar_media FROM webhooks WHERE id = $1", id).Scan(&avatar)
	if err != nil {
		return nil, mapStorageError(err)
	}
	return avatar, nil
}

// RevokeWebhook revoga um webhook (soft delete: revoked_at = NOW()). As
// mensagens já enviadas mantêm a atribuição ao webhook. ErrNotFound quando o
// webhook não existe ou já está revogado.
func RevokeWebhook(ctx context.Context, id string) error {
	result, err := GetDB().ExecContext(ctx,
		"UPDATE webhooks SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL", id)
	if err != nil {
		return fmt.Errorf("falha ao revogar webhook: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetWebhookMessage busca a mensagem que um webhook enviou com um
// external_message_id (chave de idempotência parcial). ErrNotFound quando não
// existe.
func GetWebhookMessage(ctx context.Context, webhookID, sourceID string) (models.Message, error) {
	message, err := scanMessage(GetDB().QueryRowContext(ctx,
		"SELECT "+messageColumns+" FROM messages WHERE webhook_id=$1 AND external_message_id=$2", webhookID, sourceID))
	if err != nil {
		return models.Message{}, mapStorageError(err)
	}
	return message, nil
}
