package services

import (
	"encoding/json"
	"fmt"
	"papo/internal/models"
	"papo/internal/storage"
	"strings"
	"testing"
)

// TestPluginQuotas testa tetos de chaves, bytes agregados e valor individual no Postgres.
func TestPluginQuotas(t *testing.T) {
	u := newWebhookTestUser(t)
	a, _, e := CreateApp(testCtx(), u.ID, "quota", "plugin", "Quota", models.AppCapabilities{PrivateStorage: true})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = storage.GetDB().Exec("UPDATE app_installations SET state='enabled' WHERE id=$1", a.ID); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 256; i++ {
		if _, e = storage.MutateAppStorage(testCtx(), a.ID, fmt.Sprint(i), json.RawMessage(`0`), false); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = storage.MutateAppStorage(testCtx(), a.ID, "extra", json.RawMessage(`0`), false); e != storage.ErrAppStorageQuota {
		t.Fatal(e)
	}
	if _, e = storage.MutateAppStorage(testCtx(), a.ID, "0", json.RawMessage(`1`), false); e != nil {
		t.Fatal("update na quota", e)
	}
	if _, e = storage.MutateAppStorage(testCtx(), a.ID, "0", json.RawMessage(`"`+strings.Repeat("x", 16384)+`"`), false); e != storage.ErrAppStorageQuota {
		t.Fatal(e)
	}
	if e = storage.ClearAppStorage(testCtx(), a.ID); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 64; i++ {
		if _, e = storage.MutateAppStorage(testCtx(), a.ID, fmt.Sprint(i), json.RawMessage(`"`+strings.Repeat("x", 16380)+`"`), false); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = storage.MutateAppStorage(testCtx(), a.ID, "aggregate", json.RawMessage(`"`+strings.Repeat("x", 1000)+`"`), false); e != storage.ErrAppStorageQuota {
		t.Fatal(e)
	}
}
