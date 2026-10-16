package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"papo/internal/models"
	"papo/internal/storage"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureSurfaceSource substitui somente a descoberta de plugins; nenhum runtime é necessário.
type fixtureSurfaceSource map[string]PluginSurfaceBundle

// Resolve entrega os metadados aprovados que a PR 5 fornecerá em produção.
func (s fixtureSurfaceSource) Resolve(_ context.Context, id string) (PluginSurfaceBundle, error) {
	b, ok := s[id]
	if !ok {
		return b, ErrAppNotFound
	}
	return b, nil
}

// newSurfaceFixture prepara uma instalação sem token e uma pasta de plugin estática.
func newSurfaceFixture(t *testing.T) (models.AppInstallation, models.User, fixtureSurfaceSource) {
	t.Helper()
	u := newWebhookTestUser(t)
	a, token, err := CreateApp(testCtx(), u.ID, "surfaces", "plugin", "Superfícies", models.AppCapabilities{SendMessages: true, PostAppEmbeds: true})
	if err != nil || token != "" {
		t.Fatalf("instalação: %v", err)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "ui"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"ui/index.html": "<p>canal</p>", "ui/result.html": "<p>embed</p>", "plugin.wasm": "\x00asm\x01\x00\x00\x00"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	source := fixtureSurfaceSource{a.ID: {Directory: dir, Digest: "fixture-v1", Enabled: true, Surfaces: []AppSurfaceDeclaration{
		{ID: "channel", Kind: "channel", DefaultName: "app_" + randHex(4), Source: "bundled", Entry: "ui/index.html"},
		{ID: "result", Kind: "embed", Source: "bundled", Entry: "ui/result.html", Size: models.AppSurfaceSize{Width: 480, Height: 320}},
	}}}
	SetPluginSurfaceSource(source)
	t.Cleanup(func() { SetPluginSurfaceSource(nil) })
	return a, u, source
}

// TestAppSurfaceLifecycle verifica reconciliação, customizações, ACL e exclusões com snapshot.
func TestAppSurfaceLifecycle(t *testing.T) {
	a, u, source := newSurfaceFixture(t)
	b := source[a.ID]
	changes, err := CreateAppChannels(testCtx(), u.ID, a.ID, b.Surfaces)
	if err != nil || len(changes) != 1 || changes[0].Before != nil || changes[0].After.Type != "app" {
		t.Fatalf("create: %+v %v", changes, err)
	}
	ch := *changes[0].After
	if ch.App.Entry != "/apps/"+a.ID+"/ui/index.html" {
		t.Fatal(ch.App)
	}
	if has, err := storage.AppHasScope(testCtx(), a.ID, ch.ID); err != nil || has {
		t.Fatal("canal app recebeu escopo", err)
	}
	again, err := CreateAppChannels(testCtx(), u.ID, a.ID, b.Surfaces)
	if err != nil || len(again) != 0 {
		t.Fatal(again, err)
	}
	name := "renamed_" + randHex(4)
	if _, err := UpdateChannel(testCtx(), u.ID, ch.ID, name, nil); err != nil {
		t.Fatal(err)
	}
	moved, err := ChangeChannelPosition(testCtx(), u.ID, ch.ID, ch.Position, 1)
	if err != nil {
		t.Fatal(err)
	}
	role, err := storage.CreateRole(testCtx(), "appread_"+randHex(4), nil, models.RolePermissions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.AssignUserRole(testCtx(), u.ID, role.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateChannelPermissions(testCtx(), u.ID, ch.ID, role.ID, models.ChannelPermission{ReadChannel: true}); err != nil {
		t.Fatal(err)
	}
	outsider := newWebhookTestUser(t)
	for _, actor := range []struct {
		id   string
		want bool
	}{{u.ID, true}, {outsider.ID, false}} {
		summaries, err := ListChannels(testCtx(), actor.id)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, s := range summaries {
			if s.ID == ch.ID {
				found = true
				if (s.App != nil) != actor.want {
					t.Fatalf("ACL: %+v", s)
				}
				raw, _ := json.Marshal(s)
				if strings.Contains(string(raw), `"views"`) {
					t.Fatal(string(raw))
				}
			}
		}
		if !found {
			t.Fatal("canal ausente")
		}
	}
	if err := DeleteChannel(testCtx(), u.ID, ch.ID); !errors.Is(err, ErrAppChannelManaged) {
		t.Fatal(err)
	}
	if err := ValidateChannelTypeChange(testCtx(), ch.ID, "text"); !errors.Is(err, ErrAppChannelManaged) {
		t.Fatal(err)
	}
	if _, err := CreateChannel(testCtx(), u.ID, "manual_"+randHex(4), "app", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	for _, operation := range []func() error{
		func() error { _, err := CreateMessage(testCtx(), ch.ID, u.ID, "sem chat", "", nil, nil); return err },
		func() error { _, err := ListMessages(testCtx(), ch.ID, u.ID, nil, ""); return err },
		func() error { _, err := ListPinnedMessages(testCtx(), ch.ID, u.ID); return err },
		func() error { return CanTypeChannel(testCtx(), ch.ID, u.ID) },
		func() error { _, _, err := CreateWebhook(testCtx(), u.ID, ch.ID, "hook"); return err },
		func() error { return AddAppScope(testCtx(), u.ID, a.ID, ch.ID) },
	} {
		if err := operation(); !errors.Is(err, ErrPermissionDenied) {
			t.Fatal("conversa permitida em app", err)
		}
	}
	if err := CanConnectVoice(testCtx(), ch.ID, u.ID); !errors.Is(err, ErrInvalidChannelType) {
		t.Fatal(err)
	}
	b.ApprovedOrigins = []string{"https://8.8.8.8"}
	b.Surfaces[0].Source = "remote"
	b.Surfaces[0].Entry = "https://8.8.8.8/map"
	source[a.ID] = b
	changes, err = ReconcileAppChannels(testCtx(), u.ID, a.ID, b.Surfaces)
	if err != nil || len(changes) != 1 || changes[0].Before == nil || changes[0].After.ID != ch.ID {
		t.Fatal(changes, err)
	}
	after := changes[0].After
	if after.Name != name || after.Position != moved.Position || !after.Permissions[role.ID].ReadChannel || after.App.Entry != "https://8.8.8.8/map" {
		t.Fatal(after)
	}
	b.Surfaces = b.Surfaces[1:]
	source[a.ID] = b
	changes, err = ReconcileAppChannels(testCtx(), u.ID, a.ID, b.Surfaces)
	if err != nil || len(changes) != 1 || changes[0].After != nil {
		t.Fatal(changes, err)
	}
	allowed, err := ChannelReadersSnapshot(testCtx(), *changes[0].Before, []string{u.ID, outsider.ID})
	if err != nil || !allowed[u.ID] || allowed[outsider.ID] {
		t.Fatal(allowed, err)
	}
	b.Surfaces = append(b.Surfaces, AppSurfaceDeclaration{ID: "new", Kind: "channel", DefaultName: "fresh_" + randHex(4), Source: "bundled", Entry: "ui/index.html"})
	source[a.ID] = b
	changes, err = CreateAppChannels(testCtx(), u.ID, a.ID, b.Surfaces)
	if err != nil || len(changes) != 1 || changes[0].After.ID == ch.ID {
		t.Fatal(changes, err)
	}
	b.Enabled = false
	source[a.ID] = b
	changes, err = DeleteAppChannels(testCtx(), u.ID, a.ID)
	if err != nil || len(changes) != 1 {
		t.Fatal(changes, err)
	}
	b.Enabled = true
	source[a.ID] = b
	if _, err := CreateAppChannels(testCtx(), u.ID, a.ID, b.Surfaces); err != nil {
		t.Fatal(err)
	}
	changes, err = RevokeAppWithChannelChanges(testCtx(), u.ID, a.ID)
	if err != nil || len(changes) != 1 || changes[0].After != nil {
		t.Fatal(changes, err)
	}
	if _, err := storage.GetChannelByID(testCtx(), changes[0].Before.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
}

// TestAppSurfaceValidation cobre origens, entradas, declarações e atomicidade das falhas.
func TestAppSurfaceValidation(t *testing.T) {
	a, u, source := newSurfaceFixture(t)
	b := source[a.ID]
	bad := []AppSurfaceDeclaration{
		{ID: "x", Kind: "channel", DefaultName: "x", Source: "bundled", Entry: "ui/../plugin.wasm"},
		{ID: "x", Kind: "channel", DefaultName: "x", Source: "remote", Entry: "https://8.8.8.8/map"},
		{ID: "x", Kind: "embed", Source: "bundled", Entry: "ui/result.html", Size: models.AppSurfaceSize{Width: 99999, Height: 320}},
		{ID: "x", Kind: "tab", Source: "bundled", Entry: "ui/index.html"},
	}
	for _, s := range bad {
		if _, err := CreateAppChannels(testCtx(), u.ID, a.ID, []AppSurfaceDeclaration{s}); err == nil {
			t.Fatal("declaração aceita", s)
		}
	}
	duplicate := b.Surfaces[0]
	if _, err := CreateAppChannels(testCtx(), u.ID, a.ID, []AppSurfaceDeclaration{duplicate, duplicate}); err == nil {
		t.Fatal("id duplicado")
	}
	other := duplicate
	other.ID = "other"
	if _, err := CreateAppChannels(testCtx(), u.ID, a.ID, []AppSurfaceDeclaration{duplicate, other}); err == nil {
		t.Fatal("nomes duplicados")
	}
	var count int
	if err := storage.GetDB().QueryRow("SELECT count(*) FROM app_channels WHERE app_installation_id=$1", a.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("rollback", count, err)
	}
	for _, raw := range []string{"http://8.8.8.8/", "https://localhost/", "https://127.0.0.1/", "https://10.0.0.1/", "https://172.16.0.1/", "https://192.168.1.1/", "https://[::1]/", "https://[fc00::1]/", "https://papo.com/", strings.Repeat("a", 2049)} {
		if _, err := validateRemoteSurfaceURL(testCtx(), raw, "https://papo.com/"); err == nil {
			t.Fatal(raw)
		}
	}
	if _, err := validateRemoteSurfaceURL(testCtx(), "https://[2606:4700:4700:0:0:0:0:1111]/", "https://[2606:4700:4700::1111]/"); err == nil {
		t.Fatal("mesmo host IPv6 aceito por grafia alternativa")
	}
	for _, raw := range []string{"https://8.8.8.8/map", "https://[2606:4700:4700::1111]/"} {
		if _, err := validateRemoteSurfaceURL(testCtx(), raw, "https://papo.com/"); err != nil {
			t.Fatal(raw, err)
		}
	}
}

// TestAppEmbedsLifecycle cobre remetentes, capacidade, superfície declarada e histórico inerte.
func TestAppEmbedsLifecycle(t *testing.T) {
	a, u, source := newSurfaceFixture(t)
	ch := newWebhookTestChannel(t, "text")
	if err := AddAppScope(testCtx(), u.ID, a.ID, ch.ID); err != nil {
		t.Fatal(err)
	}
	input := EmbedInput{SourceType: "app", AppSurfaceID: "result", Title: "Resultado"}
	m, created, err := CreatePluginMessage(testCtx(), a.ID, ch.ID, "", "", "", nil, []EmbedInput{input})
	if err != nil || !created || len(m.Embeds) != 1 || m.Embeds[0].SourceType != "app" || m.Embeds[0].Entry == "" {
		t.Fatalf("embed: %+v %v", m, err)
	}
	if m.Embeds[0].Entry != "/apps/"+a.ID+"/ui/result.html" {
		t.Fatal(m.Embeds[0])
	}
	if _, err := CreateMessage(testCtx(), ch.ID, u.ID, "", "", nil, []EmbedInput{input}); !errors.Is(err, ErrInvalidEmbed) {
		t.Fatal("humano", err)
	}
	hook, _, err := CreateWebhook(testCtx(), u.ID, ch.ID, "hook")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := CreateWebhookMessage(testCtx(), hook, nil, "", "", "", nil, []EmbedInput{input}); !errors.Is(err, ErrInvalidEmbed) {
		t.Fatal("webhook", err)
	}
	for _, inputs := range [][]EmbedInput{{{SourceType: "app", AppSurfaceID: "missing"}}, {{SourceType: "app", AppSurfaceID: "channel"}}, {input, input}, {{SourceType: "app", AppSurfaceID: "result", URL: "https://8.8.8.8/"}}} {
		if _, _, err := CreatePluginMessage(testCtx(), a.ID, ch.ID, "", "", "", nil, inputs); err == nil {
			t.Fatal("embed inválido aceito")
		}
	}
	if _, err := UpdateApp(testCtx(), u.ID, a.ID, nil, &models.AppCapabilities{SendMessages: true}, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CreatePluginMessage(testCtx(), a.ID, ch.ID, "", "", "", nil, []EmbedInput{input}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatal("capacidade", err)
	}
	b := source[a.ID]
	b.Enabled = false
	source[a.ID] = b
	list, err := ListMessages(testCtx(), ch.ID, u.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range list.Messages {
		if msg.ID == m.ID {
			found = true
			e := msg.Embeds[0]
			if e.Surface == nil || e.Entry != "" || e.Poster != "" || e.URL != nil || e.EmbedURL != nil {
				t.Fatal("embed ativo", e)
			}
		}
	}
	if !found {
		t.Fatal("histórico perdido")
	}
	b.Enabled = true
	source[a.ID] = b
	if err := RevokeApp(testCtx(), u.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	items, err := ListMessageEmbeds(testCtx(), []string{m.ID})
	if err != nil || items[m.ID][0].Entry != "" {
		t.Fatal(items, err)
	}
}
