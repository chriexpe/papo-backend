package storage

import (
	"encoding/json"
	"errors"
	"os"
	"papo/internal/models"
	"path/filepath"
	"strings"
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

// TestAppStorageQuotaMigrationPreservesValues mantém valores ampliados durante down/up.
func TestAppStorageQuotaMigrationPreservesValues(t *testing.T) {
	ctx := testCtx()
	u := newTestUser(t)
	a, e := CreateApp(ctx, "quota_migration", "plugin", "Quota", u.ID, models.AppCapabilities{PrivateStorage: true}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = GetDB().Exec(`UPDATE app_installations SET state='enabled',storage_quota='{"max_value_bytes":32768}' WHERE id=$1`, a.ID); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(strings.Repeat("x", 20000))
	if _, e = MutateAppStorage(ctx, a.ID, "large", raw, false); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(filepath.Join(migrationsDir, "023_plugin_storage_quotas.sql"))
	if e != nil {
		t.Fatal(e)
	}
	parts := strings.SplitN(string(data), "-- +goose Down", 2)
	if _, e = GetDB().ExecContext(ctx, parts[1]); e != nil {
		t.Fatal("down", e)
	}
	saved, e := GetAppStorage(ctx, a.ID, "large")
	if e != nil || string(saved.Value) != string(raw) {
		t.Fatal("valor perdido", e)
	}
	if _, e = GetDB().ExecContext(ctx, parts[0]); e != nil {
		t.Fatal("up", e)
	}
	saved, e = GetAppStorage(ctx, a.ID, "large")
	if e != nil || string(saved.Value) != string(raw) {
		t.Fatal("valor perdido no up", e)
	}
}
