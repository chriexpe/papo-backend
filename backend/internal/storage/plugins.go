package storage

import (
	"context"
	"encoding/json"
	"papo/internal/models"
)

// FindPlugin consulta a instalação persistente associada à pasta, inclusive desativada.
func FindPlugin(ctx context.Context, key string) (models.AppInstallation, error) {
	return scanApp(GetDB().QueryRowContext(ctx, "SELECT "+appColumns+" FROM app_installations WHERE app_key=$1 AND kind='plugin' AND revoked_at IS NULL ORDER BY created_at,id LIMIT 1", key))
}

// PluginApproval reúne os metadados que o consentimento aprovou exatamente.
type PluginApproval struct {
	Key, Name, Actor, Digest, Version                        string
	Settings, Origins, Permissions, Surfaces, ExplicitScopes json.RawMessage
	MemoryMB                                                 int
	StorageQuota                                             models.AppStorageQuotaOverride
	Capabilities                                             models.AppCapabilities
	Scopes                                                   []string
	ExpectedRevision                                         int64
	Avatar                                                   *string
}

// SavePlugin aplica grants, settings e escopos na mesma transação com revisão otimista.
func SavePlugin(ctx context.Context, p PluginApproval) (models.AppInstallation, error) {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return models.AppInstallation{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,1))", p.Key); err != nil {
		return models.AppInstallation{}, err
	}
	a, err := scanApp(tx.QueryRowContext(ctx, "SELECT "+appColumns+" FROM app_installations WHERE app_key=$1 AND kind='plugin' AND revoked_at IS NULL ORDER BY created_at,id LIMIT 1 FOR UPDATE", p.Key))
	if err == ErrNotFound {
		if p.ExpectedRevision != 0 {
			return a, ErrUniqueViolation
		}
		a, err = scanApp(tx.QueryRowContext(ctx, "INSERT INTO app_installations(app_key,kind,name,created_by) VALUES($1,'plugin',$2,$3) RETURNING "+appColumns, p.Key, p.Name, p.Actor))
	}
	if err != nil {
		return a, err
	}
	if a.PluginRevision != p.ExpectedRevision {
		return a, ErrUniqueViolation
	}
	caps, _ := json.Marshal(p.Capabilities)
	_, err = tx.ExecContext(ctx, `UPDATE app_installations SET name=$2,plugin_digest=$3,version=$4,settings=$5,approved_origins=$6,approved_permissions=$7,plugin_surfaces=$8,max_memory_mb=$9,capabilities=$10,state='enabled',lifecycle_pending=true,explicit_scopes=$12,avatar_media=$11,plugin_revision=plugin_revision+1,storage_quota=$13 WHERE id=$1`, a.ID, p.Name, p.Digest, p.Version, p.Settings, p.Origins, p.Permissions, p.Surfaces, p.MemoryMB, string(caps), p.Avatar, p.ExplicitScopes, string(mustQuotaJSON(p.StorageQuota)))
	if err != nil {
		return a, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM app_channel_scopes WHERE app_installation_id=$1", a.ID); err != nil {
		return a, err
	}
	creator := p.Actor
	if creator == "" {
		creator = a.CreatedBy
	}
	for _, scope := range p.Scopes {
		if _, err = tx.ExecContext(ctx, "INSERT INTO app_channel_scopes(app_installation_id,channel_id,created_by) VALUES($1,$2,$3)", a.ID, scope, creator); err != nil {
			return a, err
		}
	}
	if err = tx.Commit(); err != nil {
		return a, err
	}
	return GetApp(ctx, a.ID)
}

// SetPluginState fecha grants quando desativado e mantém grants aprovados na quarentena.
func SetPluginState(ctx context.Context, id, state string) error {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE app_installations SET state=$2,lifecycle_pending=false,plugin_revision=plugin_revision+1, capabilities=CASE WHEN $2 IN ('disabled','removed') THEN '{}'::jsonb ELSE capabilities END WHERE id=$1 AND kind='plugin'", id, state); err != nil {
		return err
	}
	if state == "disabled" || state == "removed" {
		if _, err = tx.ExecContext(ctx, "DELETE FROM app_channel_scopes WHERE app_installation_id=$1", id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM app_delivery_state WHERE installation_id=$1", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// QuarantinePlugin não sobrescreve aprovação concorrente de outro digest.
func QuarantinePlugin(ctx context.Context, id, digest string) error {
	_, err := GetDB().ExecContext(ctx, "UPDATE app_installations SET state='quarantined',lifecycle_pending=false,plugin_revision=plugin_revision+1 WHERE id=$1 AND plugin_digest=$2 AND state='enabled'", id, digest)
	return err
}

// PluginScopes lista o conjunto atual para vincular ao consentimento e ao hot reload.
func PluginScopes(ctx context.Context, id string) ([]string, error) {
	rows, err := GetDB().QueryContext(ctx, "SELECT channel_id FROM app_channel_scopes WHERE app_installation_id=$1 ORDER BY channel_id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListPluginInstallations permite reconciliar remoções mesmo após reiniciar o processo.
func ListPluginInstallations(ctx context.Context) ([]models.AppInstallation, error) {
	rows, err := GetDB().QueryContext(ctx, "SELECT "+appColumns+" FROM app_installations WHERE kind='plugin' AND revoked_at IS NULL ORDER BY created_at DESC,id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.AppInstallation{}
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// FinishPlugin publica a ativação somente depois dos hooks e do callback com sucesso.
func FinishPlugin(ctx context.Context, id string, revision int64) error {
	r, err := GetDB().ExecContext(ctx, "UPDATE app_installations SET lifecycle_pending=false,plugin_revision=plugin_revision+1 WHERE id=$1 AND plugin_revision=$2 AND state='enabled' AND lifecycle_pending", id, revision)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return ErrUniqueViolation
	}
	return nil
}

// mustQuotaJSON serializa um tipo fechado, sem dados arbitrários.
func mustQuotaJSON(q models.AppStorageQuotaOverride) []byte { b, _ := json.Marshal(q); return b }
