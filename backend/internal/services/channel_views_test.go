package services

import (
	"errors"
	"papo/internal/models"
	"papo/internal/storage"
	"strings"
	"testing"
)

// newChannelViewTest prepara instalação passiva escopada sem credencial.
func newChannelViewTest(t *testing.T) (models.AppInstallation, models.Channel, models.User) {
	t.Helper()
	u := newWebhookTestUser(t)
	ch := newWebhookTestChannel(t, "text")
	a, token, err := CreateApp(testCtx(), u.ID, "map", "view", "Mapa", models.AppCapabilities{AttachViews: true})
	if err != nil || token != "" {
		t.Fatalf("instalação: %v", err)
	}
	if err := AddAppScope(testCtx(), u.ID, a.ID, ch.ID); err != nil {
		t.Fatal(err)
	}
	return a, ch, u
}

// TestChannelViewLifecycle cobre CRUD, dois canais, ordenação, revogação e cascatas.
func TestChannelViewLifecycle(t *testing.T) {
	a, ch, u := newChannelViewTest(t)
	save := func(channel, id, raw string, pos int) models.ChannelAppView {
		t.Helper()
		v, err := SaveChannelView(testCtx(), u.ID, channel, id, a.ID, raw, "https://papo.com/", pos)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	v := save(ch.ID, "", "https://8.8.8.8/map", 2)
	first := save(ch.ID, "", "https://1.1.1.1/map", 0)
	views, err := ListChannelViews(testCtx(), u.ID, ch.ID)
	if err != nil || len(views) != 2 || views[0].ID != first.ID {
		t.Fatalf("lista: %+v %v", views, err)
	}
	updated := save(ch.ID, v.ID, "https://8.8.4.4/new", 1)
	if updated.CreatedBy != u.ID || updated.ID != v.ID || updated.URL == v.URL {
		t.Fatal(updated)
	}
	second := newWebhookTestChannel(t, "voice")
	if err := AddAppScope(testCtx(), u.ID, a.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	save(second.ID, "", "https://1.1.1.1/other", 0)
	summaries, err := ListChannels(testCtx(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, s := range summaries {
		if s.ID == ch.ID || s.ID == second.ID {
			found++
			if len(s.Views) == 0 {
				t.Fatal("views ausentes")
			}
		}
	}
	if found != 2 {
		t.Fatal(found)
	}
	if err := DeleteChannelView(testCtx(), u.ID, ch.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := DeleteChannelView(testCtx(), u.ID, ch.ID, first.ID); !errors.Is(err, ErrChannelViewNotFound) {
		t.Fatal(err)
	}
	if err := RemoveAppScope(testCtx(), u.ID, a.ID, ch.ID); err != nil {
		t.Fatal(err)
	}
	views, err = storage.ListChannelViews(testCtx(), ch.ID)
	if err != nil || len(views) != 0 {
		t.Fatalf("remoção escopo: %v %v", views, err)
	}
	if err := RevokeApp(testCtx(), u.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := storage.GetDB().QueryRow("SELECT count(*) FROM channel_app_views WHERE app_installation_id=$1", a.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("revogação: %d %v", count, err)
	}
	a, ch, u = newChannelViewTest(t)
	save(ch.ID, "", "https://8.8.8.8/", 0)
	if err := storage.DeleteChannel(testCtx(), ch.ID); err != nil {
		t.Fatal(err)
	}
	if err := storage.GetDB().QueryRow("SELECT count(*) FROM channel_app_views WHERE channel_id=$1", ch.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("canal: %d %v", count, err)
	}
}

// TestChannelViewDenials cobre tipo, capacidade, escopo, ACL privada e não leitores.
func TestChannelViewDenials(t *testing.T) {
	a, ch, u := newChannelViewTest(t)
	save := func(actor, channel, app, id string) error {
		_, err := SaveChannelView(testCtx(), actor, channel, id, app, "https://8.8.8.8/", "https://papo.com/", 0)
		return err
	}
	for _, kind := range []string{"remote", "wasm", "view"} {
		bad, _, err := CreateApp(testCtx(), u.ID, "bad", kind, "Bad", models.AppCapabilities{AttachViews: kind != "view"})
		if err != nil {
			t.Fatal(err)
		}
		if err := save(u.ID, ch.ID, bad.ID, ""); !errors.Is(err, ErrPermissionDenied) {
			t.Fatal(err)
		}
	}
	unscoped := newWebhookTestChannel(t, "text")
	if err := save(u.ID, unscoped.ID, a.ID, ""); !errors.Is(err, ErrPermissionDenied) {
		t.Fatal(err)
	}
	v, err := SaveChannelView(testCtx(), u.ID, ch.ID, "", a.ID, "https://8.8.8.8/", "https://papo.com/", 0)
	if err != nil {
		t.Fatal(err)
	}
	role, err := storage.CreateRole(testCtx(), "view_"+randHex(4), nil, models.RolePermissions{ManageApps: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.UpdateChannelPermissions(testCtx(), ch.ID, role.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.AssignUserRole(testCtx(), u.ID, role.ID); err != nil {
		t.Fatal(err)
	}
	outsider := newWebhookTestUser(t)
	if err := save(outsider.ID, ch.ID, a.ID, ""); !errors.Is(err, ErrPermissionDenied) {
		t.Fatal(err)
	}
	if err := save(outsider.ID, ch.ID, a.ID, v.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Fatal(err)
	}
	if err := DeleteChannelView(testCtx(), outsider.ID, ch.ID, v.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Fatal(err)
	}
	if _, err := ListChannelViews(testCtx(), outsider.ID, ch.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Fatal(err)
	}
	summaries, err := ListChannels(testCtx(), outsider.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range summaries {
		if s.ID == ch.ID && len(s.Views) != 0 {
			t.Fatal("view privada exposta")
		}
	}
	if _, err := UpdateApp(testCtx(), u.ID, a.ID, nil, &models.AppCapabilities{}, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	views, err := ListChannelViews(testCtx(), u.ID, ch.ID)
	if err != nil || len(views) != 0 {
		t.Fatal(views, err)
	}
	if err := save(u.ID, ch.ID, a.ID, v.ID); !errors.Is(err, ErrPermissionDenied) {
		t.Fatal(err)
	}
	if err := DeleteChannelView(testCtx(), u.ID, ch.ID, v.ID); err != nil {
		t.Fatal(err)
	}
}

// TestChannelViewURLs exige HTTPS público e host diferente do servidor configurado.
func TestChannelViewURLs(t *testing.T) {
	for _, raw := range []string{"http://8.8.8.8/", "https://localhost/", "https://localhost./", "https://x.localhost/", "https://127.0.0.1/", "https://10.1.2.3/", "https://172.16.0.1/", "https://192.168.1.1/", "https://169.254.169.254/", "https://100.64.0.1/", "https://[::1]/", "https://[fc00::1]/", "https://[::ffff:127.0.0.1]/", "https://PAPO.COM:443/", "https://papo.com./", "https://user@8.8.8.8/", "https://8.8.8.8:8443/", "file:///map", strings.Repeat("x", 2049)} {
		t.Run(raw[:min(len(raw), 65)], func(t *testing.T) {
			if _, err := validateChannelViewURL(testCtx(), raw, "https://papo.com:8080/api"); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("aceitou %s: %v", raw, err)
			}
		})
	}
	if _, err := validateChannelViewURL(testCtx(), "https://8.8.8.8/", "invalid"); err == nil {
		t.Fatal("base inválida")
	}
	for _, raw := range []string{"https://8.8.8.8/map", "https://[2606:4700:4700::1111]/"} {
		if _, err := validateChannelViewURL(testCtx(), raw, "https://papo.com/"); err != nil {
			t.Fatal(raw, err)
		}
	}
}
