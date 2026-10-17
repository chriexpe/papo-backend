package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"papo/internal/config"
	"papo/internal/models"
)

// ErrAppStorageQuota indica que a instalação esgotou seu espaço persistente.
var ErrAppStorageQuota = errors.New("quota de armazenamento do app excedida")

// AppStorageEntry mantém o valor e a revisão monotônica de uma chave existente.
type AppStorageEntry struct {
	Value    json.RawMessage `json:"value"`
	Revision int64           `json:"revision"`
}

// GetAppStorage consulta somente o namespace da instalação autenticada.
func GetAppStorage(ctx context.Context, id, key string) (AppStorageEntry, error) {
	var e AppStorageEntry
	err := GetDB().QueryRowContext(ctx,
		"SELECT value, revision FROM app_storage WHERE installation = $1 AND key = $2",
		id, key,
	).Scan(&e.Value, &e.Revision)
	return e, mapStorageError(err)
}

// MutateAppStorage grava (ou remove, com remove=true) uma chave respeitando a
// quota efetiva da instalação. O uso (chaves e bytes de chave + JSONB
// normalizado) fica em app_storage_usage, atualizado na mesma transação: cada
// escrita trava só a linha de uso da instalação, sem somar todas as chaves e
// sem travar app_installations. Os padrões globais e o override são lidos na
// mesma transação.
func MutateAppStorage(ctx context.Context, id, key string, value json.RawMessage, remove bool) (AppStorageEntry, error) {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return AppStorageEntry{}, err
	}
	defer tx.Rollback()

	var raw []byte
	err = tx.QueryRowContext(ctx,
		`SELECT storage_quota FROM app_installations
		 WHERE id = $1 AND kind = 'plugin' AND state = 'enabled' AND revoked_at IS NULL
		   AND capabilities @> '{"private_storage":true}'::jsonb`,
		id,
	).Scan(&raw)
	if err != nil {
		return AppStorageEntry{}, mapStorageError(err)
	}

	// Serializa as escritas da instalação pela linha de uso.
	if _, err = tx.ExecContext(ctx,
		"INSERT INTO app_storage_usage (installation) VALUES ($1) ON CONFLICT DO NOTHING", id,
	); err != nil {
		return AppStorageEntry{}, err
	}
	var keys, bytes int
	if err = tx.QueryRowContext(ctx,
		"SELECT keys, bytes FROM app_storage_usage WHERE installation = $1 FOR UPDATE", id,
	).Scan(&keys, &bytes); err != nil {
		return AppStorageEntry{}, err
	}

	// Tamanho atual da chave (0 quando não existe).
	var current int
	err = tx.QueryRowContext(ctx,
		"SELECT octet_length(key) + octet_length(value::text) FROM app_storage WHERE installation = $1 AND key = $2",
		id, key,
	).Scan(&current)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AppStorageEntry{}, err
	}

	if remove {
		if !exists {
			return AppStorageEntry{}, tx.Commit()
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM app_storage WHERE installation = $1 AND key = $2", id, key); err != nil {
			return AppStorageEntry{}, err
		}
		if err = updateAppStorageUsage(ctx, tx, id, -1, -current); err != nil {
			return AppStorageEntry{}, err
		}
		return AppStorageEntry{}, tx.Commit()
	}

	var override models.AppStorageQuotaOverride
	if err = json.Unmarshal(raw, &override); err != nil {
		return AppStorageEntry{}, err
	}
	quota, err := config.EffectiveAppStorageQuota(override)
	if err != nil {
		return AppStorageEntry{}, err
	}
	var encoded int
	if err = tx.QueryRowContext(ctx, "SELECT octet_length($1::jsonb::text)", string(value)).Scan(&encoded); err != nil {
		return AppStorageEntry{}, err
	}
	keyDelta, size := 0, len(key)+encoded
	if !exists {
		keyDelta = 1
	}
	if keys+keyDelta > quota.MaxKeys || encoded > quota.MaxValueBytes || bytes-current+size > quota.MaxTotalBytes {
		return AppStorageEntry{}, ErrAppStorageQuota
	}

	var e AppStorageEntry
	err = tx.QueryRowContext(ctx,
		`INSERT INTO app_storage (installation, key, value) VALUES ($1, $2, $3)
		 ON CONFLICT (installation, key) DO UPDATE SET value = EXCLUDED.value, revision = app_storage.revision + 1
		 RETURNING value, revision`,
		id, key, string(value),
	).Scan(&e.Value, &e.Revision)
	if err != nil {
		return e, err
	}
	if err = updateAppStorageUsage(ctx, tx, id, keyDelta, size-current); err != nil {
		return e, err
	}
	return e, tx.Commit()
}

// updateAppStorageUsage aplica a variação de chaves e bytes ao uso da instalação.
func updateAppStorageUsage(ctx context.Context, tx *sql.Tx, id string, keys, bytes int) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE app_storage_usage SET keys = keys + $2, bytes = bytes + $3 WHERE installation = $1",
		id, keys, bytes,
	)
	return err
}

// ClearAppStorage apaga todas as chaves da instalação e zera o uso.
func ClearAppStorage(ctx context.Context, id string) error {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM app_storage WHERE installation = $1", id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM app_storage_usage WHERE installation = $1", id); err != nil {
		return err
	}
	return tx.Commit()
}
