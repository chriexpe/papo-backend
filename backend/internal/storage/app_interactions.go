package storage

import (
	"context"
	"encoding/json"
	"papo/internal/models"
	"time"
)

// AppRateLimit usa o banco para compartilhar limites entre processos.
func AppRateLimit(ctx context.Context, key string, limit int) (bool, error) {
	var n int
	err := GetDB().QueryRowContext(ctx, `INSERT INTO app_rate_limits(key,window_at,count) VALUES($1,now(),1)
 ON CONFLICT(key) DO UPDATE SET count=CASE WHEN app_rate_limits.window_at <= now()-interval '1 minute' THEN 1 ELSE app_rate_limits.count+1 END,
 window_at=CASE WHEN app_rate_limits.window_at <= now()-interval '1 minute' THEN now() ELSE app_rate_limits.window_at END RETURNING count`, key).Scan(&n)
	return n <= limit, err
}

// RevokeAppUI invalida inclusive tickets já entregues e sessões em outro processo.
func RevokeAppUI(ctx context.Context, id string) error {
	_, e := GetDB().ExecContext(ctx, `UPDATE app_installations SET ui_generation=ui_generation+1 WHERE id=$1`, id)
	return e
}

// AppUIGeneration consulta a geração atual de autorização.
func AppUIGeneration(ctx context.Context, id string) (int64, error) {
	var n int64
	e := GetDB().QueryRowContext(ctx, `SELECT ui_generation FROM app_installations WHERE id=$1`, id).Scan(&n)
	return n, e
}

// SaveAppUIToken persiste somente o hash do segredo.
func SaveAppUIToken(ctx context.Context, t models.AppUIToken) error {
	_, e := GetDB().ExecContext(ctx, `INSERT INTO app_ui_tokens(id,token_hash,kind,user_id,installation_id,channel_id,surface_id,message_id,invite_id,generation,digest,expires_at,absolute_expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, t.ID, t.Hash, t.Kind, t.UserID, t.InstallationID, t.ChannelID, t.SurfaceID, t.MessageID, t.InviteID, t.Generation, t.Digest, t.ExpiresAt, t.AbsoluteExpiresAt)
	return e
}

// GetAppUIToken falha fechado para expiração, consumo ou geração substituída.
func GetAppUIToken(ctx context.Context, hash, kind, id string) (models.AppUIToken, error) {
	var t models.AppUIToken
	e := GetDB().QueryRowContext(ctx, `SELECT t.id,t.token_hash,t.kind,t.user_id,t.installation_id,t.channel_id,t.surface_id,t.message_id,t.invite_id,t.generation,t.digest,t.expires_at,t.absolute_expires_at FROM app_ui_tokens t JOIN app_installations a ON a.id=t.installation_id JOIN users u ON u.id=t.user_id AND NOT u.banned WHERE token_hash=$1 AND t.kind=$2 AND t.installation_id=$3 AND NOT consumed AND t.expires_at>now() AND t.absolute_expires_at>now() AND t.generation=a.ui_generation`, hash, kind, id).Scan(&t.ID, &t.Hash, &t.Kind, &t.UserID, &t.InstallationID, &t.ChannelID, &t.SurfaceID, &t.MessageID, &t.InviteID, &t.Generation, &t.Digest, &t.ExpiresAt, &t.AbsoluteExpiresAt)
	return t, mapStorageError(e)
}

// ConsumeAppTicket garante troca única, inclusive em requisições concorrentes.
func ConsumeAppTicket(ctx context.Context, id string) (bool, error) {
	r, e := GetDB().ExecContext(ctx, `UPDATE app_ui_tokens t SET consumed=true FROM app_installations a WHERE t.id=$1 AND a.id=t.installation_id AND t.generation=a.ui_generation AND NOT consumed AND expires_at>now() AND absolute_expires_at>now()`, id)
	if e != nil {
		return false, e
	}
	n, e := r.RowsAffected()
	return n == 1, e
}

// TouchAppSession estende a inatividade sem ultrapassar o máximo absoluto.
func TouchAppSession(ctx context.Context, id string, idle time.Duration) error {
	r, e := GetDB().ExecContext(ctx, `UPDATE app_ui_tokens t SET expires_at=LEAST(t.absolute_expires_at,now()+$2::interval) FROM app_installations a WHERE t.id=$1 AND t.installation_id=a.id AND t.generation=a.ui_generation AND a.state='enabled' AND a.revoked_at IS NULL AND NOT a.lifecycle_pending AND t.expires_at>now() AND t.absolute_expires_at>now() AND NOT t.consumed`, id, idle.String())
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e == nil && n != 1 {
		return ErrNotFound
	}
	return e
}

// RevokeUserAppUI impede que o retorno posterior do acesso ressuscite tokens recusados.
func RevokeUserAppUI(ctx context.Context, user, id string) error {
	_, e := GetDB().ExecContext(ctx, `DELETE FROM app_ui_tokens WHERE user_id=$1 AND installation_id=$2`, user, id)
	return e
}

// CleanAppInteractions remove lotes limitados sem disputar registros de outro worker.
func CleanAppInteractions(ctx context.Context) error {
	tx, e := GetDB().BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM app_ui_tokens WHERE id IN (SELECT id FROM app_ui_tokens WHERE expires_at<=now() OR absolute_expires_at<=now() OR consumed ORDER BY expires_at LIMIT 500 FOR UPDATE SKIP LOCKED)`,
		`DELETE FROM push_outbox WHERE notification_id IN (SELECT id FROM notifications WHERE kind='app_invite' AND expires_at<=now())`,
		`DELETE FROM notifications WHERE id IN (SELECT id FROM notifications WHERE kind='app_invite' AND expires_at<=now() ORDER BY expires_at LIMIT 500 FOR UPDATE SKIP LOCKED)`,
		`DELETE FROM app_rate_limits WHERE window_at<now()-interval '1 hour'`,
	} {
		if _, e = tx.ExecContext(ctx, q); e != nil {
			return e
		}
	}
	return tx.Commit()
}

// SaveAppComponents só altera mensagens pertencentes à instalação e ao canal.
func SaveAppComponents(ctx context.Context, id, channel, message string, buttons [][]models.AppButton) error {
	if buttons == nil {
		buttons = [][]models.AppButton{}
	}
	b, e := json.Marshal(buttons)
	if e != nil {
		return e
	}
	r, e := GetDB().ExecContext(ctx, `UPDATE messages SET components=$4 WHERE id=$1 AND app_installation_id=$2 AND channel_id=$3`, message, id, channel, b)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

// AppComponents retorna uma cópia de botões, desabilitando instalações indisponíveis.
func AppComponents(ctx context.Context, ids []string) (map[string][][]models.AppButton, error) {
	out := map[string][][]models.AppButton{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, e := GetDB().QueryContext(ctx, `SELECT m.id,m.components,COALESCE(a.state='enabled' AND a.revoked_at IS NULL AND NOT a.lifecycle_pending,false) FROM messages m LEFT JOIN app_installations a ON a.id=m.app_installation_id WHERE m.id=ANY($1)`, ids)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var b []byte
		var enabled bool
		if e = rows.Scan(&id, &b, &enabled); e != nil {
			return nil, e
		}
		var buttons [][]models.AppButton
		if e = json.Unmarshal(b, &buttons); e != nil {
			return nil, e
		}
		if !enabled {
			for i := range buttons {
				for j := range buttons[i] {
					buttons[i][j].Disabled = true
				}
			}
		}
		out[id] = buttons
	}
	return out, rows.Err()
}
