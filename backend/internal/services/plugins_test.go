package services

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/wasmhost"
)

// pluginFixture usa o exemplo TinyGo real, também exercitado pelo servidor local.
func pluginFixture(t *testing.T) []byte {
	t.Helper()
	b, e := os.ReadFile("../../../examples/wasm-reference/plugin.wasm")
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// pluginTestHost configura um adaptador independente e o fecha ao terminar.
func pluginTestHost(t *testing.T) *wasmhost.Host {
	t.Helper()
	h, e := wasmhost.New(testCtx(), wasmhost.DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { h.Close(context.Background()) })
	return h
}

// TestPluginPersistentStorage verifica namespace, revisões e persistência após reiniciar o runtime.
func TestPluginPersistentStorage(t *testing.T) {
	u := newWebhookTestUser(t)
	a, _, e := CreateApp(testCtx(), u.ID, "storage", "wasm", "Storage", models.AppCapabilities{PrivateStorage: true})
	if e != nil {
		t.Fatal(e)
	}
	h := pluginTestHost(t)
	if _, e = UploadPlugin(testCtx(), h, u.ID, a.ID, pluginFixture(t)); e != nil {
		t.Fatal(e)
	}
	invoke := func(host *wasmhost.Host, id, input string) string {
		t.Helper()
		out, e := InvokePlugin(testCtx(), host, id, []byte(input), nil)
		if e != nil {
			t.Fatal(e)
		}
		return string(out)
	}
	if out := invoke(h, a.ID, `{"method":"kv.put","key":"counter","value":42}`); !strings.Contains(out, `"revision":1`) {
		t.Fatal(out)
	}
	if out := invoke(h, a.ID, `{"method":"kv.put","key":"counter","value":43}`); !strings.Contains(out, `"revision":2`) {
		t.Fatal(out)
	}
	h.Close(testCtx())
	h = pluginTestHost(t)
	if out := invoke(h, a.ID, `{"method":"kv.get","key":"counter"}`); !strings.Contains(out, `"value":43`) {
		t.Fatal(out)
	}
	b, _, e := CreateApp(testCtx(), u.ID, "other", "wasm", "Other", models.AppCapabilities{PrivateStorage: true})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = UploadPlugin(testCtx(), h, u.ID, b.ID, pluginFixture(t)); e != nil {
		t.Fatal(e)
	}
	if out := invoke(h, b.ID, `{"method":"kv.get","key":"counter"}`); !strings.Contains(out, "not_found") {
		t.Fatal(out)
	}
	if out := invoke(h, a.ID, `{"method":"kv.delete","key":"counter"}`); !strings.Contains(out, `"ok":true`) {
		t.Fatal(out)
	}
	if out := invoke(h, a.ID, `{"method":"kv.get","key":"counter"}`); !strings.Contains(out, "not_found") {
		t.Fatal(out)
	}
	caps := models.AppCapabilities{}
	if _, e = UpdateApp(testCtx(), u.ID, a.ID, nil, &caps, nil, nil, ""); e != nil {
		t.Fatal(e)
	}
	if out := invoke(h, a.ID, `{"method":"kv.put","key":"counter","value":99}`); !strings.Contains(out, "forbidden") {
		t.Fatal(out)
	}
}

// TestPluginQuotas testa tetos de chaves, bytes agregados e valor individual no Postgres.
func TestPluginQuotas(t *testing.T) {
	u := newWebhookTestUser(t)
	a, _, e := CreateApp(testCtx(), u.ID, "quota", "wasm", "Quota", models.AppCapabilities{PrivateStorage: true})
	if e != nil {
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
	if _, e = storage.GetDB().Exec("DELETE FROM app_storage WHERE installation=$1", a.ID); e != nil {
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

// TestPluginMessageAuthorization compara explicitamente os serviços usados pelo REST e pelo guest.
func TestPluginMessageAuthorization(t *testing.T) {
	remote, ch, u := newAppTest(t)
	a, _, e := CreateApp(testCtx(), u.ID, "send", "wasm", "Sender", models.AppCapabilities{SendMessages: true})
	if e != nil {
		t.Fatal(e)
	}
	h := pluginTestHost(t)
	if _, e = UploadPlugin(testCtx(), h, u.ID, a.ID, pluginFixture(t)); e != nil {
		t.Fatal(e)
	}
	send := func() string {
		t.Helper()
		req, _ := json.Marshal(map[string]string{"method": "message.send", "channel_id": ch.ID, "content": "hello", "external_message_id": "dedupe"})
		out, e := InvokePlugin(testCtx(), h, a.ID, req, nil)
		if e != nil {
			t.Fatal(e)
		}
		return string(out)
	}
	if out := send(); !strings.Contains(out, "forbidden") {
		t.Fatal(out)
	}
	if e = AddAppScope(testCtx(), u.ID, a.ID, ch.ID); e != nil {
		t.Fatal(e)
	}
	if out := send(); !strings.Contains(out, `"ok":true`) {
		t.Fatal(out)
	}
	for _, id := range []string{a.ID, remote.ID} {
		for _, allowed := range []bool{false, true} {
			caps := models.AppCapabilities{SendMessages: allowed}
			if _, e = UpdateApp(testCtx(), u.ID, id, nil, &caps, nil, nil, ""); e != nil {
				t.Fatal(e)
			}
			app, e := storage.GetApp(testCtx(), id)
			if e != nil {
				t.Fatal(e)
			}
			_, _, e = CreateAppMessage(testCtx(), app, ch.ID, "", "auth", "", nil, nil)
			if id == a.ID && !allowed {
				if out := send(); !strings.Contains(out, "forbidden") {
					t.Fatal("host call ignorou capacidade", out)
				}
			}
			if allowed && e != nil || !allowed && e != ErrPermissionDenied {
				t.Fatalf("REST/Wasm %s %v: %v", id, allowed, e)
			}
		}
	}
	if e = RemoveAppScope(testCtx(), u.ID, a.ID, ch.ID); e != nil {
		t.Fatal(e)
	}
	if out := send(); !strings.Contains(out, "forbidden") {
		t.Fatal(out)
	}
	if e = RevokeApp(testCtx(), u.ID, a.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = InvokePlugin(testCtx(), h, a.ID, []byte(`{}`), nil); e != ErrAppNotFound {
		t.Fatal(e)
	}
}
