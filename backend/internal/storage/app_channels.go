package storage

import (
	"context"
	"database/sql"
	"fmt"
	"papo/internal/models"
)

// GetAppChannel consulta a superfície sem herdar escopo ou permissões do instalador.
func GetAppChannel(ctx context.Context, id string) (*models.AppChannel, error) {
	var a models.AppChannel
	err := GetDB().QueryRowContext(ctx, `SELECT ac.app_installation_id,ac.surface_id,ac.source,ac.entry,
 '/apps/'||a.id||'/avatar',a.revoked_at IS NULL FROM app_channels ac JOIN app_installations a ON a.id=ac.app_installation_id WHERE ac.channel_id=$1`, id).Scan(&a.AppInstallationID, &a.SurfaceID, &a.Source, &a.Entry, &a.AvatarURL, &a.Available)
	return &a, mapStorageError(err)
}

// appChannelsInTx captura o canal completo e sua ACL sob lock, antes de removê-lo.
func appChannelsInTx(ctx context.Context, tx *sql.Tx, id string) ([]models.Channel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.name,c.permissions,c.type,c.position,c.created_at,c.topic FROM channels c
 JOIN app_channels ac ON ac.channel_id=c.id WHERE ac.app_installation_id=$1 ORDER BY c.id FOR UPDATE OF c`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]models.Channel, 0)
	for rows.Next() {
		ch, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}

// deleteAppChannelsInTx remove os canais e retorna a ACL anterior para channel_delete.
func deleteAppChannelsInTx(ctx context.Context, tx *sql.Tx, id string) ([]models.AppChannelChange, error) {
	channels, err := appChannelsInTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	changes := make([]models.AppChannelChange, 0, len(channels))
	for _, ch := range channels {
		if err := deleteChannelInTx(ctx, tx, ch.ID); err != nil {
			return nil, err
		}
		before := ch
		changes = append(changes, models.AppChannelChange{Before: &before})
	}
	return changes, nil
}

// ReconcileAppChannels mantém IDs e customizações por surface_id em uma única transação.
// nil e lista vazia removem todos os canais. A instalação serializa enable/revoke.
func ReconcileAppChannels(ctx context.Context, id string, surfaces []models.AppSurfaceDeclaration) ([]models.AppChannelChange, error) {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var active string
	if err := tx.QueryRowContext(ctx, "SELECT id FROM app_installations WHERE id=$1 AND revoked_at IS NULL FOR UPDATE", id).Scan(&active); err != nil {
		return nil, mapStorageError(err)
	}
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", channelPositionLockKey); err != nil {
		return nil, err
	}
	existing, err := appChannelsInTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	type record struct {
		channel                models.Channel
		surface, source, entry string
	}
	old := make(map[string]record)
	for _, ch := range existing {
		var sid, source, entry string
		if err := tx.QueryRowContext(ctx, "SELECT surface_id,source,entry FROM app_channels WHERE channel_id=$1", ch.ID).Scan(&sid, &source, &entry); err != nil {
			return nil, err
		}
		old[sid] = record{ch, sid, source, entry}
	}
	changes := make([]models.AppChannelChange, 0)
	for _, surface := range surfaces {
		if surface.Kind != "channel" {
			continue
		}
		var ch models.Channel
		var before *models.Channel
		if prior, ok := old[surface.ID]; ok {
			delete(old, surface.ID)
			if prior.source == surface.Source && prior.entry == surface.Entry {
				continue
			}
			ch = prior.channel
			prev := ch
			before = &prev
			if _, err := tx.ExecContext(ctx, "UPDATE app_channels SET source=$2,entry=$3 WHERE channel_id=$1", ch.ID, surface.Source, surface.Entry); err != nil {
				return nil, err
			}
		} else {
			var count int
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM channels WHERE type<>'dm'").Scan(&count); err != nil {
				return nil, err
			}
			if count >= 500 {
				return nil, fmt.Errorf("limite de canais atingido")
			}
			row := tx.QueryRowContext(ctx, "INSERT INTO channels(name,type,position) VALUES ($1,'app',(SELECT COALESCE(MAX(position),0)+1 FROM channels)) RETURNING "+channelColumns, surface.DefaultName)
			ch, err = scanChannel(row)
			if err != nil {
				return nil, mapStorageError(err)
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO app_channels(channel_id,app_installation_id,surface_id,source,entry) VALUES ($1,$2,$3,$4,$5)", ch.ID, id, surface.ID, surface.Source, surface.Entry); err != nil {
				return nil, mapStorageError(err)
			}
		}
		ch.App = &models.AppChannel{AppInstallationID: id, SurfaceID: surface.ID, Source: surface.Source, Entry: surface.Entry, AvatarURL: "/apps/" + id + "/avatar", Available: true}
		after := ch
		changes = append(changes, models.AppChannelChange{Before: before, After: &after})
	}
	for _, prior := range old {
		if err := deleteChannelInTx(ctx, tx, prior.channel.ID); err != nil {
			return nil, err
		}
		before := prior.channel
		changes = append(changes, models.AppChannelChange{Before: &before})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return changes, nil
}

// AppSurfaceChannelIDs includes historical inline embeds, even without an app channel.
// Call before deleting app channels; authorization remains the caller's responsibility.
func AppSurfaceChannelIDs(ctx context.Context, installationID string) ([]string, error) {
	rows, err := GetDB().QueryContext(ctx, `SELECT channel_id FROM app_channels WHERE app_installation_id=$1
 UNION SELECT m.channel_id FROM embeds e JOIN message_embeds me ON me.embed_id=e.id
 JOIN messages m ON m.id=me.message_id WHERE e.app_installation_id=$1
 UNION SELECT channel_id FROM messages WHERE app_installation_id=$1 AND components<>'[]'::jsonb`, installationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
