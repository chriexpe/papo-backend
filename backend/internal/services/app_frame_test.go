package services

import (
	"encoding/json"
	"errors"
	"os"
	"papo/internal/models"
	"papo/internal/storage"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppFrameDeclarations cobre size obrigatório, tetos configuráveis, load e display.
func TestAppFrameDeclarations(t *testing.T) {
	t.Setenv("APP_EMBED_MAX_WIDTH", "560")
	t.Setenv("APP_EMBED_MAX_HEIGHT", "420")
	a, _, source := newSurfaceFixture(t)
	b := source[a.ID]
	base := b.Surfaces[1]
	for _, tc := range []struct {
		name          string
		size          models.AppSurfaceSize
		load, display string
		valid         bool
	}{
		{"defaults", models.AppSurfaceSize{Width: 480, Height: 320}, "", "", true},
		{"maximum", models.AppSurfaceSize{Width: 560, Height: 420}, "visible", "inline", true},
		{"overlay_default_click", models.AppSurfaceSize{Width: 560, Height: 420}, "", "overlay", true},
		{"overlay_visible", models.AppSurfaceSize{Width: 480, Height: 320}, "visible", "overlay", false},
		{"missing", models.AppSurfaceSize{}, "", "", false},
		{"zero_width", models.AppSurfaceSize{Width: 0, Height: 320}, "", "", false},
		{"negative", models.AppSurfaceSize{Width: 480, Height: -1}, "", "", false},
		{"wide", models.AppSurfaceSize{Width: 561, Height: 420}, "", "", false},
		{"tall", models.AppSurfaceSize{Width: 560, Height: 421}, "", "", false},
		{"bad_load", models.AppSurfaceSize{Width: 480, Height: 320}, "auto", "inline", false},
		{"bad_display", models.AppSurfaceSize{Width: 480, Height: 320}, "click", "popup", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			s.Size = tc.size
			s.Load = tc.load
			s.Display = tc.display
			out, err := validateAppSurfaces(testCtx(), b, []AppSurfaceDeclaration{s})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
			if err == nil {
				wantLoad := tc.load
				if wantLoad == "" {
					wantLoad = "click"
				}
				wantDisplay := tc.display
				if wantDisplay == "" {
					wantDisplay = "inline"
				}
				if out[0].Load != wantLoad || out[0].Display != wantDisplay || out[0].Size != tc.size {
					t.Fatal(out)
				}
			}
		})
	}
	t.Setenv("APP_EMBED_MAX_WIDTH", "800")
	t.Setenv("APP_EMBED_MAX_HEIGHT", "600")
	base.Size = models.AppSurfaceSize{Width: 800, Height: 600}
	if _, err := validateAppSurfaces(testCtx(), b, []AppSurfaceDeclaration{base}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APP_EMBED_MAX_WIDTH", "799")
	if _, err := validateAppSurfaces(testCtx(), b, []AppSurfaceDeclaration{base}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
}

// TestAppFramePosterValidation exige imagem existente de ui/, sem traversal ou links.
func TestAppFramePosterValidation(t *testing.T) {
	a, _, source := newSurfaceFixture(t)
	b := source[a.ID]
	s := b.Surfaces[1]
	if err := os.WriteFile(filepath.Join(b.Directory, "ui", "poster.png"), pngAvatarBytes(2, 2), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.Directory, "ui", "poster.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="2" height="2"></svg>`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.Directory, "ui", "invalid.png"), []byte("isto não é imagem"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, poster := range []string{"", "ui/poster.png", "ui/poster.svg"} {
		s.Poster = poster
		if _, err := validateAppSurfaces(testCtx(), b, []AppSurfaceDeclaration{s}); err != nil {
			t.Fatal(poster, err)
		}
	}
	if err := os.Symlink(filepath.Join(b.Directory, "plugin.wasm"), filepath.Join(b.Directory, "ui", "outside.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("poster.png", filepath.Join(b.Directory, "ui", "inside.png")); err != nil {
		t.Fatal(err)
	}
	for _, poster := range []string{"ui/missing.png", "ui/../plugin.wasm", "/ui/poster.png", "ui/invalid.png", "ui/index.html", "ui/outside.png", "ui/inside.png", "ui/%2e%2e/poster.png", "ui\\poster.png", "https://example.org/poster.png"} {
		s.Poster = poster
		if _, err := validateAppSurfaces(testCtx(), b, []AppSurfaceDeclaration{s}); !errors.Is(err, ErrInvalidInput) {
			t.Fatal("poster aceito", poster, err)
		}
	}
}

// assertAppFrameJSON limita o frame ao contrato sem metadados de identidade e do card nativo.
func assertAppFrameJSON(t *testing.T, e models.Embed, live bool) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	permitted := map[string]bool{"id": true, "source_type": true, "surface": true, "entry": true, "size": true, "load": true, "display": true, "poster": true, "title": true}
	for key := range payload {
		if !permitted[key] {
			t.Fatal("campo extra no frame", key, string(raw))
		}
	}
	var surface models.AppSurfaceReference
	if err := json.Unmarshal(payload["surface"], &surface); err != nil || surface.ID != "result" || surface.AppInstallationID == "" {
		t.Fatal(surface, err)
	}
	_, entry := payload["entry"]
	if entry != live {
		t.Fatal("estado da entrada", string(raw))
	}
	if !live {
		if _, poster := payload["poster"]; poster {
			t.Fatal("poster ativo após disable", string(raw))
		}
	}
	return payload
}

// TestAppFramePayloadAndNativeCards verifica política persistida, título, inércia e cards nativos.
func TestAppFramePayloadAndNativeCards(t *testing.T) {
	a, u, source := newSurfaceFixture(t)
	b := source[a.ID]
	s := &b.Surfaces[1]
	s.Size = models.AppSurfaceSize{Width: 560, Height: 420}
	s.Display = "overlay"
	s.Load = "click"
	s.Poster = "ui/poster.png"
	if err := os.WriteFile(filepath.Join(b.Directory, "ui", "poster.png"), pngAvatarBytes(2, 2), 0600); err != nil {
		t.Fatal(err)
	}
	source[a.ID] = b
	ch := newWebhookTestChannel(t, "text")
	if err := AddAppScope(testCtx(), u.ID, a.ID, ch.ID); err != nil {
		t.Fatal(err)
	}
	input := EmbedInput{SourceType: "app", AppSurfaceID: "result", Title: strings.Repeat("é", 256)}
	m, _, err := CreatePluginMessage(testCtx(), a.ID, ch.ID, "", "", "", nil, []EmbedInput{input})
	if err != nil {
		t.Fatal(err)
	}
	e := m.Embeds[0]
	payload := assertAppFrameJSON(t, e, true)
	if string(payload["size"]) != `{"width":560,"height":420}` || string(payload["display"]) != `"overlay"` || string(payload["load"]) != `"click"` || e.Poster != "/apps/"+a.ID+"/ui/fixture-v1/poster.png" {
		t.Fatal(payload, e)
	}
	var width, height int
	var load, display, poster string
	if err := storage.GetDB().QueryRow(`SELECT app_width,app_height,app_load,app_display,app_poster FROM embeds WHERE id=$1`, e.ID).Scan(&width, &height, &load, &display, &poster); err != nil || width != 560 || height != 420 || load != "click" || display != "overlay" || poster != "ui/poster.png" {
		t.Fatal(width, height, load, display, poster, err)
	}
	input.Title += "é"
	if _, _, err := CreatePluginMessage(testCtx(), a.ID, ch.ID, "", "", "", nil, []EmbedInput{input}); !errors.Is(err, ErrInvalidEmbed) {
		t.Fatal("título longo", err)
	}
	input.Title = ""
	for _, bad := range []EmbedInput{{SourceType: "app", AppSurfaceID: "result", Description: "card"}, {SourceType: "app", AppSurfaceID: "result", Thumbnail: &EmbedMediaInput{URL: "https://8.8.8.8/image.png"}}, {SourceType: "app", AppSurfaceID: "result", Color: "#112233"}} {
		if _, _, err := CreatePluginMessage(testCtx(), a.ID, ch.ID, "", "", "", nil, []EmbedInput{bad}); !errors.Is(err, ErrInvalidEmbed) {
			t.Fatal("campos nativos aceitos em frame", err)
		}
	}
	withoutTitle, _, err := CreatePluginMessage(testCtx(), a.ID, ch.ID, "", "", "", nil, []EmbedInput{input})
	if err != nil {
		t.Fatal(err)
	}
	if _, has := assertAppFrameJSON(t, withoutTitle.Embeds[0], true)["title"]; has {
		t.Fatal("título não enviado apareceu")
	}
	// O remetente app continua podendo criar cards nativos sem post_app_embeds.
	if _, err := UpdateApp(testCtx(), u.ID, a.ID, nil, &models.AppCapabilities{SendMessages: true}, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	native, _, err := CreatePluginMessage(testCtx(), a.ID, ch.ID, "", "texto", "", nil, []EmbedInput{{Title: "Card nativo", Description: "Texto", Color: "#112233", Fields: []EmbedFieldInput{{Name: "Campo", Value: "Valor"}}}})
	if err != nil || native.Embeds[0].SourceType != "custom" || native.Embeds[0].Description == nil {
		t.Fatal(native, err)
	}
	nativeJSON, err := json.Marshal(native.Embeds[0])
	if err != nil || strings.Contains(string(nativeJSON), `"surface"`) {
		t.Fatal(string(nativeJSON), err)
	}
	b.Enabled = false
	source[a.ID] = b
	listed, err := ListMessageEmbeds(testCtx(), []string{m.ID})
	if err != nil {
		t.Fatal(err)
	}
	inert := assertAppFrameJSON(t, listed[m.ID][0], false)
	for _, key := range []string{"size", "load", "display", "title", "surface"} {
		if string(inert[key]) != string(payload[key]) {
			t.Fatal("snapshot alterado", key)
		}
	}
	b.Enabled = true
	source[a.ID] = b
	// Reduzir o máximo não lança um frame histórico que agora excede o teto.
	t.Setenv("APP_EMBED_MAX_WIDTH", "559")
	listed, err = ListMessageEmbeds(testCtx(), []string{m.ID})
	if err != nil {
		t.Fatal(err)
	}
	assertAppFrameJSON(t, listed[m.ID][0], false)
}
