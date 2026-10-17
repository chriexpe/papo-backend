package storage

import (
	"encoding/json"
	"errors"
	"papo/internal/models"
	"testing"
)

// TestAppStorageQuotaTotal considera chaves e JSONB normalizado, não só bytes enviados.
func TestAppStorageQuotaTotal(t *testing.T) {
	ctx := testCtx()
	u := newTestUser(t)
	a, e := CreateApp(ctx, "quota_total", "plugin", "Quota", u.ID, models.AppCapabilities{PrivateStorage: true}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = GetDB().Exec(`UPDATE app_installations SET state='enabled',storage_quota='{"max_keys":5,"max_total_bytes":20,"max_value_bytes":12}' WHERE id=$1`, a.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = MutateAppStorage(ctx, a.ID, "abcdefghij", json.RawMessage(`"12345678"`), false); e != nil {
		t.Fatal(e)
	}
	if _, e = MutateAppStorage(ctx, a.ID, "x", json.RawMessage(`1`), false); !errors.Is(e, ErrAppStorageQuota) {
		t.Fatal("total excedido aceito", e)
	}
	if _, e = MutateAppStorage(ctx, a.ID, "abcdefghij", nil, true); e != nil {
		t.Fatal(e)
	}
	if _, e = MutateAppStorage(ctx, a.ID, "x", json.RawMessage(`1`), false); e != nil {
		t.Fatal(e)
	}
}

// TestAppStorageUsageMatchesRows mantém o uso incremental igual à soma real
// das chaves após criar, sobrescrever com tamanhos diferentes e remover.
func TestAppStorageUsageMatchesRows(t *testing.T) {
	ctx := testCtx()
	u := newTestUser(t)
	a, err := CreateApp(ctx, "quota_usage", "plugin", "Uso", u.ID, models.AppCapabilities{PrivateStorage: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = GetDB().Exec(`UPDATE app_installations SET state='enabled' WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		key    string
		value  string
		remove bool
	}{
		{"a", `{"x": 1}`, false}, {"b", `"curto"`, false}, {"a", `{"x": [1, 2, 3, 4, 5]}`, false},
		{"b", `1`, false}, {"c", `null`, false}, {"a", ``, true}, {"inexistente", ``, true}, {"c", `"de novo"`, false},
	}
	for _, s := range steps {
		if _, err = MutateAppStorage(ctx, a.ID, s.key, json.RawMessage(s.value), s.remove); err != nil {
			t.Fatal(s.key, err)
		}
		var keys, bytes, wantKeys, wantBytes int
		if err = GetDB().QueryRow(`SELECT keys, bytes FROM app_storage_usage WHERE installation=$1`, a.ID).Scan(&keys, &bytes); err != nil {
			t.Fatal(err)
		}
		if err = GetDB().QueryRow(`SELECT count(*), COALESCE(sum(octet_length(key)+octet_length(value::text)),0)
			FROM app_storage WHERE installation=$1`, a.ID).Scan(&wantKeys, &wantBytes); err != nil {
			t.Fatal(err)
		}
		if keys != wantKeys || bytes != wantBytes {
			t.Fatalf("depois de %s: uso %d/%d, real %d/%d", s.key, keys, bytes, wantKeys, wantBytes)
		}
	}
}
