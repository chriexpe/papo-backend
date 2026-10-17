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
