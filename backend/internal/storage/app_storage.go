package storage

import (
	"context"
	"encoding/json"
	"errors"
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
	err := GetDB().QueryRowContext(ctx, "SELECT value,revision FROM app_storage WHERE installation=$1 AND key=$2", id, key).Scan(&e.Value, &e.Revision)
	return e, mapStorageError(err)
}

// MutateAppStorage serializa a quota com o lock da instalação, inclusive entre processos.
// Cada instalação aceita até 256 chaves e 1 MiB de chaves mais JSON normalizado.
func MutateAppStorage(ctx context.Context, id, key string, value json.RawMessage, remove bool) (AppStorageEntry, error) {
	tx, err := GetDB().BeginTx(ctx, nil)
	if err != nil {
		return AppStorageEntry{}, err
	}
	defer tx.Rollback()
	var active string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM app_installations WHERE id=$1 AND kind='plugin' AND state='enabled' AND capabilities @> '{"private_storage":true}'::jsonb AND revoked_at IS NULL FOR UPDATE`, id).Scan(&active); err != nil {
		return AppStorageEntry{}, mapStorageError(err)
	}
	if remove {
		_, err = tx.ExecContext(ctx, "DELETE FROM app_storage WHERE installation=$1 AND key=$2", id, key)
		if err != nil {
			return AppStorageEntry{}, err
		}
		return AppStorageEntry{}, tx.Commit()
	}
	var count, size, encoded int
	if err = tx.QueryRowContext(ctx, `SELECT count(*), COALESCE(sum(octet_length(key)+octet_length(value::text)),0) FROM app_storage WHERE installation=$1 AND key<>$2`, id, key).Scan(&count, &size); err != nil {
		return AppStorageEntry{}, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT octet_length($1::jsonb::text)", string(value)).Scan(&encoded); err != nil {
		return AppStorageEntry{}, err
	}
	if count >= 256 || encoded > 16384 || size+len(key)+encoded > 1<<20 {
		return AppStorageEntry{}, ErrAppStorageQuota
	}
	var e AppStorageEntry
	err = tx.QueryRowContext(ctx, `INSERT INTO app_storage(installation,key,value) VALUES($1,$2,$3)
 ON CONFLICT(installation,key) DO UPDATE SET value=EXCLUDED.value,revision=app_storage.revision+1
 RETURNING value,revision`, id, key, string(value)).Scan(&e.Value, &e.Revision)
	if err != nil {
		return e, err
	}
	return e, tx.Commit()
}
