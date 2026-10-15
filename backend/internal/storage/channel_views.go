package storage

import (
	"context"
	"papo/internal/models"
)

const channelViewColumns = "id, app_installation_id, channel_id, url, position, created_by, created_at"

// scanChannelView lê os campos públicos de uma view.
func scanChannelView(row rowScanner) (models.ChannelAppView, error) {
	var v models.ChannelAppView
	err := row.Scan(&v.ID, &v.AppInstallationID, &v.ChannelID, &v.URL, &v.Position, &v.CreatedBy, &v.CreatedAt)
	return v, mapStorageError(err)
}

// ListChannelViews lista apenas views de instalações ativas com capacidade atual.
func ListChannelViews(ctx context.Context, channel string) ([]models.ChannelAppView, error) {
	rows, err := GetDB().QueryContext(ctx, `SELECT v.id,v.app_installation_id,v.channel_id,v.url,v.position,v.created_by,v.created_at
 FROM channel_app_views v JOIN app_installations a ON a.id=v.app_installation_id
 WHERE v.channel_id=$1 AND a.revoked_at IS NULL AND a.kind='view'
 AND a.capabilities->>'attach_views'='true' ORDER BY v.position,v.created_at,v.id`, channel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]models.ChannelAppView, 0)
	for rows.Next() {
		v, err := scanChannelView(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetChannelView busca uma view dentro do canal solicitado.
func GetChannelView(ctx context.Context, channel, id string) (models.ChannelAppView, error) {
	return scanChannelView(GetDB().QueryRowContext(ctx, "SELECT "+channelViewColumns+" FROM channel_app_views WHERE channel_id=$1 AND id=$2", channel, id))
}

// SaveChannelView serializa escrita com revogação, edição de capacidades e remoção de escopo.
// A FK composta impede que um vínculo removido deixe views órfãs.
func SaveChannelView(ctx context.Context, actor, app, channel, id, url string, position int) (models.ChannelAppView, error) {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return models.ChannelAppView{}, err
	}
	defer tx.Rollback()
	var locked string
	err = tx.QueryRowContext(ctx, `SELECT id FROM app_installations WHERE id=$1 AND kind='view'
 AND revoked_at IS NULL AND capabilities->>'attach_views'='true' FOR SHARE`, app).Scan(&locked)
	if err != nil {
		return models.ChannelAppView{}, mapStorageError(err)
	}
	err = tx.QueryRowContext(ctx, `SELECT channel_id FROM app_channel_scopes WHERE app_installation_id=$1 AND channel_id=$2 FOR SHARE`, app, channel).Scan(&locked)
	if err != nil {
		return models.ChannelAppView{}, mapStorageError(err)
	}
	var v models.ChannelAppView
	if id == "" {
		v, err = scanChannelView(tx.QueryRowContext(ctx, "INSERT INTO channel_app_views (app_installation_id,channel_id,url,position,created_by) VALUES ($1,$2,$3,$4,$5) RETURNING "+channelViewColumns, app, channel, url, position, actor))
	} else {
		v, err = scanChannelView(tx.QueryRowContext(ctx, "UPDATE channel_app_views SET url=$4,position=$5 WHERE id=$1 AND app_installation_id=$2 AND channel_id=$3 RETURNING "+channelViewColumns, id, app, channel, url, position))
	}
	if err != nil {
		return v, err
	}
	return v, tx.Commit()
}

// DeleteChannelView remove somente a view identificada no canal informado.
func DeleteChannelView(ctx context.Context, channel, id string) error {
	r, err := GetDB().ExecContext(ctx, "DELETE FROM channel_app_views WHERE channel_id=$1 AND id=$2", channel, id)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
