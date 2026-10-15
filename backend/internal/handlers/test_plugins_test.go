package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"papo/internal/models"
	"papo/internal/plugins"
	"papo/internal/storage"
)

// TestPluginRoutes exercita artefato real, autenticação, limites e isolamento de erros.
func TestPluginRoutes(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	owner := webhookTestServer(t, e)
	app, _ := createAppViaRoute(t, e, owner, "wasm")
	path := "/apps/" + app.ID
	guest, err := os.ReadFile("../../../examples/wasm-reference/plugin.wasm")
	if err != nil {
		t.Fatal(err)
	}
	_, unprivileged := registerAndLogin(t, e)
	for _, method := range []string{http.MethodPut, http.MethodPost} {
		endpoint := path + "/artifact"
		if method == http.MethodPost {
			endpoint = path + "/trigger"
		}
		for _, cookie := range []string{"", unprivileged} {
			rec := do(t, e, method, endpoint, guest, authCookie(cookie))
			want := 403
			if cookie == "" {
				want = 401
			}
			if rec.Code != want {
				t.Fatalf("auth %d: %s", rec.Code, rec.Body.String())
			}
		}
	}
	for _, kind := range []string{"remote", "view"} {
		other, _ := createAppViaRoute(t, e, owner, kind)
		if rec := do(t, e, http.MethodPut, "/apps/"+other.ID+"/artifact", guest, authCookie(owner)); rec.Code != 400 {
			t.Fatalf("tipo %s: %d", kind, rec.Code)
		}
	}
	if rec := do(t, e, http.MethodPost, path+"/trigger", []byte(`{}`), authCookie(owner)); rec.Code != 400 {
		t.Fatalf("sem artefato: %d", rec.Code)
	}
	for _, bad := range [][]byte{[]byte("invalid"), make([]byte, plugins.MaxArtifact+1)} {
		rec := do(t, e, http.MethodPut, path+"/artifact", bad, authCookie(owner))
		if rec.Code != 400 && rec.Code != 413 {
			t.Fatalf("upload inválido: %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := do(t, e, http.MethodPut, path+"/artifact", guest, authCookie(owner)); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if rec := do(t, e, http.MethodPatch, path, []byte(`{"capabilities":{"private_storage":true,"send_messages":true}}`), authCookie(owner)); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	for _, input := range []string{`{"method":"kv.put","key":"x","value":42}`, `{"method":"kv.get","key":"x"}`} {
		rec := do(t, e, http.MethodPost, path+"/trigger", []byte(input), authCookie(owner))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
			t.Fatalf("trigger %d %s", rec.Code, rec.Body.String())
		}
	}
	channel := createChannelFor(t, "wasm_"+randHex(4))
	scopeAppViaRoute(t, e, owner, app.ID, channel.ID)
	input := fmt.Sprintf(`{"method":"message.send","channel_id":%q,"content":"Wasm","external_message_id":"wasm-route"}`, channel.ID)
	rec := do(t, e, http.MethodPost, path+"/trigger", []byte(input), authCookie(owner))
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var result struct {
		OK     bool                         `json:"ok"`
		Result models.MessageWithAttachment `json:"result"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &result); err != nil || !result.OK || result.Result.AppInstallationID == nil || *result.Result.AppInstallationID != app.ID {
		t.Fatalf("message %s %v", rec.Body.String(), err)
	}
	if referenced, err := storage.MediaIsReferenced(context.Background(), *mustPluginApp(t, app.ID).ArtifactSHA); err != nil || !referenced {
		t.Fatalf("GC não preserva artefato: %v", err)
	}
	evil, err := os.ReadFile("../wasmhost/testdata/adversarial.wasm")
	if err != nil {
		t.Fatal(err)
	}
	if rec := do(t, e, http.MethodPut, path+"/artifact", evil, authCookie(owner)); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	for i := 0; i < 3; i++ {
		rec = do(t, e, http.MethodPost, path+"/trigger", []byte(`{"op":"trap"}`), authCookie(owner))
		if rec.Code != 422 {
			t.Fatalf("trap %d %s", rec.Code, rec.Body.String())
		}
	}
	rec = do(t, e, http.MethodPost, path+"/trigger", []byte(`{}`), authCookie(owner))
	if rec.Code != 409 {
		t.Fatalf("quarantine %d", rec.Code)
	}
	if rec := do(t, e, http.MethodPut, path+"/artifact", guest, authCookie(owner)); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	rec = do(t, e, http.MethodPost, path+"/trigger", []byte(`{"method":"kv.get","key":"x"}`), authCookie(owner))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"value":42`) {
		t.Fatalf("recovery %d %s", rec.Code, rec.Body.String())
	}
}

// mustPluginApp consulta metadados após upload, sem reutilizar a resposta de instalação.
func mustPluginApp(t *testing.T, id string) models.AppInstallation {
	t.Helper()
	a, e := storage.GetApp(context.Background(), id)
	if e != nil {
		t.Fatal(e)
	}
	return a
}
