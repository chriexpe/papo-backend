package handlers

import (
	"context"
	"encoding/json"
	"github.com/gorilla/websocket"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/storage"
	hub "papo/internal/websocket"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAppBridgeRoutes prova CORS opaco, autenticação separada e edição em tempo real.
func TestAppBridgeRoutes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PLUGINS_DIR", dir)
	src := "../../../examples/plugins/reference"
	if err := filepath.WalkDir(src, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, name)
		dst := filepath.Join(dir, "reference", rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0700)
		}
		b, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0600)
	}); err != nil {
		t.Fatal(err)
	}
	cleanServers(context.Background())
	e := newApp()
	e.Use(middleware.CORS([]string{"https://client.example"}))
	owner := webhookTestServer(t, e)
	targetID, target := registerAndLogin(t, e)
	text := createChannelFor(t, "interactions_"+randHex(4))
	rec := do(t, e, "POST", "/plugins/rescan", nil, authCookie(owner))
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	opts := services.PluginOptions{Settings: map[string]any{"target_channel": text.ID}}
	body, _ := json.Marshal(opts)
	rec = do(t, e, "POST", "/plugins/reference/preview", body, authCookie(owner))
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var p services.PluginPreview
	json.Unmarshal(rec.Body.Bytes(), &p)
	opts.Consent = p.Consent
	opts.ExpiresAt = p.ExpiresAt
	body, _ = json.Marshal(opts)
	rec = do(t, e, "POST", "/plugins/reference/enable", body, authCookie(owner))
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var a models.AppInstallation
	json.Unmarshal(rec.Body.Bytes(), &a)
	var appChannel string
	if err := storage.GetDB().QueryRow(`SELECT channel_id FROM app_channels WHERE app_installation_id=$1`, a.ID).Scan(&appChannel); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/interactions", "/apps/" + a.ID + "/launch"} {
		if got := do(t, e, "POST", path, []byte(`{}`), nil); got.Code != 401 {
			t.Fatal("sem sessão", got.Code)
		}
	}
	req := httptest.NewRequest("OPTIONS", "/apps/"+a.ID+"/rpc", nil)
	req.Header.Set("Origin", "null")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Authorization,Content-Type")
	out := httptest.NewRecorder()
	e.ServeHTTP(out, req)
	if out.Code != 204 || out.Header().Get("Access-Control-Allow-Origin") != "*" || out.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal(out.Code, out.Header())
	}
	launch := []byte(`{"surface_id":"home","channel_id":"` + appChannel + `"}`)
	rec = do(t, e, "POST", "/apps/"+a.ID+"/launch", launch, authCookie(target))
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var ticket struct {
		Ticket  string            `json:"ticket"`
		Surface models.AppChannel `json:"surface"`
	}
	json.Unmarshal(rec.Body.Bytes(), &ticket)
	if ticket.Surface.SurfaceID != "home" || ticket.Surface.AppInstallationID != a.ID || !strings.HasPrefix(ticket.Surface.Entry, "/apps/"+a.ID+"/ui/") {
		t.Fatalf("launch metadata: %+v", ticket.Surface)
	}
	sessionBody := []byte(`{"ticket":"` + ticket.Ticket + `"}`)
	request := func(path string, b []byte, bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(string(b)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "null")
		r.AddCookie(authCookie(owner))
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		o := httptest.NewRecorder()
		e.ServeHTTP(o, r)
		if o.Header().Get("Access-Control-Allow-Origin") != "*" || o.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Fatal("CORS", o.Header())
		}
		return o
	}
	rec = request("/apps/"+a.ID+"/session", sessionBody, "")
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var session map[string]string
	json.Unmarshal(rec.Body.Bytes(), &session)
	if got := request("/apps/"+a.ID+"/session", sessionBody, ""); got.Code != 401 {
		t.Fatal("replay", got.Code)
	}
	rpcBody := []byte(`{"method":"identity","params":{}}`)
	if got := request("/apps/"+a.ID+"/rpc", rpcBody, ""); got.Code != 401 {
		t.Fatal("cookie virou bearer", got.Code)
	}
	rec = request("/apps/"+a.ID+"/rpc", rpcBody, session["token"])
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), targetID) {
		t.Fatal("usuário não verificado", rec.Code, rec.Body.String())
	}
	// Conecta leitor antes da ação: message e message_edit precisam carregar components.
	go hub.GetHub().Run()
	server := httptest.NewServer(e)
	defer server.Close()
	header := http.Header{}
	header.Set("Cookie", "Auth="+target)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws", header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	readWSMessage(t, conn)
	body = []byte(`{"app_installation_id":"` + a.ID + `","channel_id":"` + appChannel + `","kind":"user_action","action_id":"convidar","target_user_id":"` + targetID + `"}`)
	rec = do(t, e, "POST", "/interactions", body, authCookie(owner))
	if rec.Code != 202 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	readEvent := func(kind string) map[string]json.RawMessage {
		t.Helper()
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			var v map[string]json.RawMessage
			if json.Unmarshal(data, &v) != nil {
				t.Fatal(string(data))
			}
			if string(v["type"]) == `"`+kind+`"` {
				return v
			}
		}
	}
	msg := readEvent("message")
	if !strings.Contains(string(msg["components"]), "revanche") {
		t.Fatal("message sem botões", msg)
	}
	var msgID string
	json.Unmarshal(msg["id"], &msgID)
	body = []byte(`{"app_installation_id":"` + a.ID + `","channel_id":"` + text.ID + `","kind":"button","message_id":"` + msgID + `","component_id":"revanche"}`)
	rec = do(t, e, "POST", "/interactions", body, authCookie(owner))
	if rec.Code != 202 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	edit := readEvent("message_edit")
	if !strings.Contains(string(edit["components"]), "revanche") {
		t.Fatal("message_edit sem botões", edit)
	}
	body = []byte(`{"app_installation_id":"` + a.ID + `","channel_id":"` + text.ID + `","kind":"button","message_id":"` + msgID + `","component_id":"inexistente"}`)
	if got := do(t, e, "POST", "/interactions", body, authCookie(owner)); got.Code != 400 {
		t.Fatal("botão inexistente", got.Code, got.Body.String())
	}
}
