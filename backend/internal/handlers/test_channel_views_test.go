package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"papo/internal/models"
	"testing"
)

// TestChannelViewRoutes verifica CRUD pelo roteador e independência de manage_apps.
func TestChannelViewRoutes(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	owner := webhookTestServer(t, e)
	ch := createChannelFor(t, "view_"+randHex(4))
	app, _ := createAppViaRoute(t, e, owner, "view")
	capBody := []byte(`{"capabilities":{"attach_views":true}}`)
	if rec := do(t, e, http.MethodPatch, "/apps/"+app.ID, capBody, authCookie(owner)); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	scopeAppViaRoute(t, e, owner, app.ID, ch.ID)
	path := "/channels/" + ch.ID + "/views"
	body, _ := json.Marshal(map[string]any{"app_installation_id": app.ID, "url": "https://8.8.8.8/map", "position": 0})
	rec := do(t, e, http.MethodPost, path, body, authCookie(owner))
	if rec.Code != 201 {
		t.Fatalf("criar: %d %s", rec.Code, rec.Body.String())
	}
	var view models.ChannelAppView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		target := path
		if method == http.MethodPut || method == http.MethodDelete {
			target += "/" + view.ID
		}
		if rec := do(t, e, method, target, body, nil); rec.Code != 401 {
			t.Fatalf("auth: %d %s", rec.Code, rec.Body.String())
		}
		id, cookie := registerAndLogin(t, e)
		role := createRoleFor(t, models.RolePermissions{ManageChannels: true})
		assignRoleToUser(t, id, role.ID)
		if rec := do(t, e, method, target, body, authCookie(cookie)); rec.Code != 403 {
			t.Fatalf("permissão: %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := do(t, e, http.MethodGet, path, nil, authCookie(owner)); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if rec := do(t, e, http.MethodPut, path+"/"+view.ID, []byte(`{"url":"https://1.1.1.1/new","position":2}`), authCookie(owner)); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	for _, bad := range []string{`{`, `{"url":"http://8.8.8.8/"}`, `{"url":"https://papo.com/"}`, `{"url":"https://127.0.0.1/"}`, `{"url":"https://8.8.8.8/","position":-1}`} {
		if rec := do(t, e, http.MethodPut, path+"/"+view.ID, []byte(bad), authCookie(owner)); rec.Code != 400 {
			t.Fatalf("inválido: %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := do(t, e, http.MethodDelete, path+"/"+view.ID, nil, authCookie(owner)); rec.Code != 204 {
		t.Fatal(rec.Body.String())
	}
	if rec := do(t, e, http.MethodPut, path+"/"+view.ID, body, authCookie(owner)); rec.Code != 404 {
		t.Fatal(rec.Body.String())
	}
}

// TestChannelViewPrivateRoutes impede gestores sem leitura de anexar ou descobrir views privadas.
func TestChannelViewPrivateRoutes(t *testing.T) {
	cleanServers(context.Background())
	e := newApp()
	owner := webhookTestServer(t, e)
	ch := createChannelFor(t, "private_view_"+randHex(4))
	app, _ := createAppViaRoute(t, e, owner, "view")
	if rec := do(t, e, http.MethodPatch, "/apps/"+app.ID, []byte(`{"capabilities":{"attach_views":true}}`), authCookie(owner)); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	scopeAppViaRoute(t, e, owner, app.ID, ch.ID)
	path := "/channels/" + ch.ID + "/views"
	body, _ := json.Marshal(map[string]any{"app_installation_id": app.ID, "url": "https://8.8.8.8/private"})
	rec := do(t, e, http.MethodPost, path, body, authCookie(owner))
	if rec.Code != 201 {
		t.Fatal(rec.Body.String())
	}
	var view models.ChannelAppView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	role := createRoleFor(t, models.RolePermissions{ManageApps: true})
	id, cookie := registerAndLogin(t, e)
	assignRoleToUser(t, id, role.ID)
	rec = do(t, e, http.MethodPut, "/channels/"+ch.ID+"/permissions/"+role.ID, []byte(`{"read_channel":false}`), authCookie(owner))
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		target := path
		if method == http.MethodPut || method == http.MethodDelete {
			target += "/" + view.ID
		}
		if rec := do(t, e, method, target, body, authCookie(cookie)); rec.Code != 403 {
			t.Fatalf("ACL %s: %d %s", method, rec.Code, rec.Body.String())
		}
	}
	rec = do(t, e, http.MethodGet, "/channels", nil, authCookie(cookie))
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var channels channelListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &channels); err != nil {
		t.Fatal(err)
	}
	for _, c := range channels.Channels {
		if c.ID == ch.ID && len(c.Views) != 0 {
			t.Fatal("URL privada exposta")
		}
	}
	rec = do(t, e, http.MethodGet, "/channels", nil, authCookie(owner))
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &channels); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range channels.Channels {
		if c.ID == ch.ID && len(c.Views) == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("dono sem view")
	}
}
