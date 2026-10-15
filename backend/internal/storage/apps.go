package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"papo/internal/models"
)

// appColumns exclui a credencial das respostas públicas.
const appColumns = "id, app_key, kind, name, display_name, avatar_media, capabilities, created_by, created_at, revoked_at"

// scanApp lê a instalação e decodifica suas capacidades.
func scanApp(row rowScanner, secret ...*string) (models.AppInstallation, error) {
	var a models.AppInstallation
	var caps []byte
	targets := []any{&a.ID, &a.AppKey, &a.Kind, &a.Name, &a.DisplayName, &a.AvatarMedia, &caps, &a.CreatedBy, &a.CreatedAt, &a.RevokedAt}
	if len(secret) > 0 {
		targets = append(targets, secret[0])
	}
	if err := row.Scan(targets...); err != nil {
		return a, mapStorageError(err)
	}
	err := json.Unmarshal(caps, &a.Capabilities)
	return a, err
}

// CreateApp persiste somente o hash da credencial; modos locais não possuem token.
func CreateApp(ctx context.Context, key, kind, name, actor string, caps models.AppCapabilities, hash *string) (models.AppInstallation, error) {
	data, err := json.Marshal(caps)
	if err != nil {
		return models.AppInstallation{}, err
	}
	return scanApp(GetDB().QueryRowContext(ctx, "INSERT INTO app_installations (app_key,kind,name,created_by,capabilities,secret_hash) VALUES ($1,$2,$3,$4,$5,$6) RETURNING "+appColumns, key, kind, name, actor, string(data), hash))
}

// GetApp busca inclusive instalações revogadas para preservar a identidade.
func GetApp(ctx context.Context, id string) (models.AppInstallation, error) {
	return scanApp(GetDB().QueryRowContext(ctx, "SELECT "+appColumns+" FROM app_installations WHERE id=$1", id))
}

// GetActiveAppByHash resolve exclusivamente credenciais remotas ativas.
func GetActiveAppByHash(ctx context.Context, hash string) (models.AppInstallation, string, error) {
	var stored string
	a, err := scanApp(GetDB().QueryRowContext(ctx, "SELECT "+appColumns+", secret_hash FROM app_installations WHERE secret_hash=$1 AND kind='remote' AND revoked_at IS NULL", hash), &stored)
	if err != nil {
		return a, "", err
	}
	return a, stored, nil
}

// ListApps pagina instalações ativas por data e UUID, sem depender do instalador.
func ListApps(ctx context.Context, since *time.Time, lastID string, asc bool, limit int) ([]models.AppInstallation, error) {
	query := "SELECT " + appColumns + " FROM app_installations WHERE revoked_at IS NULL"
	args := []any{}
	op, order := "<", "DESC"
	if asc {
		op, order = ">", "ASC"
	}
	if since != nil {
		args = append(args, *since)
		if lastID != "" {
			query += " AND (created_at,id) " + op + " ($1,$2::uuid)"
			args = append(args, lastID)
		} else {
			query += " AND created_at > $1"
		}
	}
	args = append(args, limit)
	query += " ORDER BY created_at " + order + ", id " + order + fmt.Sprintf(" LIMIT $%d", len(args))
	rows, err := GetDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]models.AppInstallation, 0)
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateApp atualiza somente os campos presentes, sem reativar instalações.
func UpdateApp(ctx context.Context, id string, name, display, avatar any, caps *models.AppCapabilities) (models.AppInstallation, error) {
	sets := []string{}
	args := []any{id}
	for _, field := range []struct {
		name  string
		value any
	}{{"name", name}, {"display_name", display}, {"avatar_media", avatar}} {
		if field.value != nil {
			args = append(args, field.value)
			sets = append(sets, fmt.Sprintf("%s=$%d", field.name, len(args)))
		}
	}
	if caps != nil {
		data, err := json.Marshal(caps)
		if err != nil {
			return models.AppInstallation{}, err
		}
		args = append(args, string(data))
		sets = append(sets, fmt.Sprintf("capabilities=$%d", len(args)))
	}
	if len(sets) == 0 {
		return scanApp(GetDB().QueryRowContext(ctx, "SELECT "+appColumns+" FROM app_installations WHERE id=$1 AND revoked_at IS NULL", id))
	}
	return scanApp(GetDB().QueryRowContext(ctx, "UPDATE app_installations SET "+strings.Join(sets, ",")+" WHERE id=$1 AND revoked_at IS NULL RETURNING "+appColumns, args...))
}

// RotateAppToken substitui o hash de uma instalação remota ativa.
func RotateAppToken(ctx context.Context, id, hash string) error {
	r, err := GetDB().ExecContext(ctx, "UPDATE app_installations SET secret_hash=$2 WHERE id=$1 AND kind='remote' AND revoked_at IS NULL", id, hash)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeApp revoga a credencial e remove escopos na mesma transação.
func RevokeApp(ctx context.Context, id string) error {
	_, err := RevokeAppWithViewChannels(ctx, id)
	return err
}

// RevokeAppWithViewChannels captura os canais afetados sob o lock da instalação,
// antes de apagar views e escopos; devolve os IDs somente depois do commit.
func RevokeAppWithViewChannels(ctx context.Context, id string) ([]string, error) {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, "UPDATE app_installations SET revoked_at=NOW() WHERE id=$1 AND revoked_at IS NULL", id)
	if err != nil {
		return nil, err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	rows, err := tx.QueryContext(ctx, "DELETE FROM channel_app_views WHERE app_installation_id=$1 RETURNING channel_id", id)
	if err != nil {
		return nil, err
	}
	channels := make([]string, 0)
	seen := make(map[string]bool)
	for rows.Next() {
		var channel string
		if err := rows.Scan(&channel); err != nil {
			rows.Close()
			return nil, err
		}
		if !seen[channel] {
			channels = append(channels, channel)
			seen[channel] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// A revogação descarta o backlog; snapshots compartilhados são podados pelo worker.
	if _, err = tx.ExecContext(ctx, "DELETE FROM app_delivery_state WHERE installation_id=$1", id); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM app_channel_scopes WHERE app_installation_id=$1", id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return channels, nil
}

// AddAppScope impede adicionar escopo após a revogação, inclusive em concorrência.
func AddAppScope(ctx context.Context, id, channel, actor string) error {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active string
	if err = tx.QueryRowContext(ctx, "SELECT id FROM app_installations WHERE id=$1 AND revoked_at IS NULL FOR UPDATE", id).Scan(&active); err != nil {
		return mapStorageError(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO app_channel_scopes (app_installation_id,channel_id,created_by) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING", id, channel, actor); err != nil {
		return mapStorageError(err)
	}
	return tx.Commit()
}

// RemoveAppScope remove o vínculo de forma idempotente.
func RemoveAppScope(ctx context.Context, id, channel string) error {
	_, err := GetDB().ExecContext(ctx, "DELETE FROM app_channel_scopes WHERE app_installation_id=$1 AND channel_id=$2", id, channel)
	return err
}

// AppHasScope consulta a ACL própria de máquina; não herda permissões do instalador.
func AppHasScope(ctx context.Context, id, channel string) (bool, error) {
	var allowed bool
	err := GetDB().QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM app_channel_scopes WHERE app_installation_id=$1 AND channel_id=$2)", id, channel).Scan(&allowed)
	return allowed, err
}

// GetAppMessage busca a chave de idempotência enquanto a mensagem existir.
func GetAppMessage(ctx context.Context, id, key string) (models.Message, error) {
	m, err := scanMessage(GetDB().QueryRowContext(ctx, "SELECT "+messageColumns+" FROM messages WHERE app_installation_id=$1 AND external_message_id=$2", id, key))
	return m, mapStorageError(err)
}
