package storage

import (
	"context"
	"encoding/json"
	"papo/internal/models"
)

// CreateAppInvite grava notificação e push na mesma transação e respeita mute.
func CreateAppInvite(ctx context.Context, v models.AppInvite, push string) error {
	tx, e := GetDB().BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	payload, e := json.Marshal(v)
	if e != nil {
		return e
	}
	r, e := tx.ExecContext(ctx, `INSERT INTO notifications(id,user_id,kind,app_installation_id,payload,expires_at,invite_status) SELECT $1,$2,'app_invite',$3,$4,$5,'pending' WHERE NOT EXISTS(SELECT 1 FROM app_user_settings WHERE user_id=$2 AND installation_id=$3 AND invites_muted)`, v.ID, v.UserID, v.InstallationID, payload, v.ExpiresAt)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	if e = CreatePushJobInTx(tx, ctx, v.UserID, &v.ID, push); e != nil {
		return e
	}
	return tx.Commit()
}

// GetAppInvite somente expõe convites ainda válidos ao destinatário.
func GetAppInvite(ctx context.Context, id, user string) (models.AppInvite, error) {
	var v models.AppInvite
	var b []byte
	var status string
	e := GetDB().QueryRowContext(ctx, `SELECT payload,invite_status FROM notifications WHERE id=$1 AND user_id=$2 AND kind='app_invite' AND expires_at>now()`, id, user).Scan(&b, &status)
	if e != nil {
		return v, mapStorageError(e)
	}
	e = json.Unmarshal(b, &v)
	v.UserID = user
	v.Status = status
	return v, e
}

// DecideAppInvite serializa a primeira decisão, permitindo sua repetição idempotente.
func DecideAppInvite(ctx context.Context, id, user, status string) error {
	r, e := GetDB().ExecContext(ctx, `UPDATE notifications SET invite_status=$3,read=true WHERE id=$1 AND user_id=$2 AND kind='app_invite' AND expires_at>now() AND invite_status IN ('pending',$3)`, id, user, status)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAppInviteMute altera somente a preferência pessoal, sem concessão de capacidades.
func SetAppInviteMute(ctx context.Context, user, id string, muted bool) error {
	_, e := GetDB().ExecContext(ctx, `INSERT INTO app_user_settings(user_id,installation_id,invites_muted) VALUES($1,$2,$3) ON CONFLICT(user_id,installation_id) DO UPDATE SET invites_muted=$3,updated_at=now()`, user, id, muted)
	return e
}
