package storage

import (
	"context"
	"papo/internal/models"
)

// AppPublicProfiles seleciona somente os quatro campos permitidos pelo ABI.
func AppPublicProfiles(ctx context.Context, ids []string) ([]models.AppUserProfile, error) {
	rows, e := GetDB().QueryContext(ctx, `SELECT id, username, COALESCE(NULLIF(nickname, ''), username),
		 CASE WHEN avatar_media IS NOT NULL THEN '/users/' || id || '/avatar' END
		 FROM users WHERE id = ANY($1) AND NOT banned`, ids)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []models.AppUserProfile{}
	for rows.Next() {
		var p models.AppUserProfile
		if e = rows.Scan(&p.ID, &p.Username, &p.DisplayName, &p.AvatarURL); e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AppProfileChannels não inclui canais históricos de embeds: somente canais próprios ou com escopo atual.
func AppProfileChannels(ctx context.Context, id string) ([]string, error) {
	rows, e := GetDB().QueryContext(ctx, `SELECT channel_id FROM app_channels WHERE app_installation_id=$1 UNION SELECT channel_id FROM app_channel_scopes WHERE app_installation_id=$1`, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			return nil, e
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
