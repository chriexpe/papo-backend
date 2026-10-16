package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papo/internal/models"
	"papo/internal/pluginbundle"
	"papo/internal/plugins"
	"papo/internal/storage"
	"papo/internal/wasmhost"
)

// copyPluginFixture copia somente arquivos do exemplo para uma pasta descartável.
func copyPluginFixture(t *testing.T, parent, id string) string {
	t.Helper()
	src := filepath.Join("../../../examples/plugins", id)
	dst := filepath.Join(parent, id)
	e := filepath.WalkDir(src, func(name string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, _ := filepath.Rel(src, name)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		b, e := os.ReadFile(name)
		if e != nil {
			return e
		}
		return os.WriteFile(target, b, 0600)
	})
	if e != nil {
		t.Fatal(e)
	}
	return dst
}

// managerFixture usa banco real, módulos reais e grants obtidos exclusivamente por consentimento.
func managerFixture(t *testing.T, hot bool) (*PluginManager, models.User, models.Channel) {
	t.Helper()
	u := newWebhookTestUser(t)
	ch := newWebhookTestChannel(t, "text")
	c, e := pluginbundle.LoadConfig()
	if e != nil {
		t.Fatal(e)
	}
	c.Directory = t.TempDir()
	c.HotReload = hot
	copyPluginFixture(t, c.Directory, "reference")
	copyPluginFixture(t, c.Directory, "bluemap")
	h, e := wasmhost.New(testCtx(), wasmhost.DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { h.Close(context.Background()); SetPluginSurfaceSource(nil) })
	m, e := NewPluginManager(c, h, PluginEffects{})
	if e != nil {
		t.Fatal(e)
	}
	SetPluginSurfaceSource(m)
	if e = m.Rescan(testCtx()); e != nil {
		t.Fatal(e)
	}
	return m, u, ch
}

// approvedOptions vincula exatamente os mesmos valores ao preview e ao enable/PATCH.
func approvedOptions(t *testing.T, m *PluginManager, actor, id string, o PluginOptions) PluginOptions {
	t.Helper()
	p, e := m.Preview(testCtx(), actor, id, o)
	if e != nil {
		t.Fatal(e)
	}
	o.Consent = p.Consent
	o.ExpiresAt = p.ExpiresAt
	return o
}

// enableReference cria uma instalação com o canal escolhido explicitamente no consentimento.
func enableReference(t *testing.T, m *PluginManager, u models.User, ch models.Channel) models.AppInstallation {
	t.Helper()
	o := PluginOptions{Settings: map[string]any{"target_channel": ch.ID}}
	o = approvedOptions(t, m, u.ID, "reference", o)
	a, e := m.Enable(testCtx(), u.ID, "reference", o)
	if e != nil {
		t.Fatal(e)
	}
	return a
}

// pluginRequestTest exercita um host call dentro do módulo TinyGo de referência.
func pluginRequestTest(t *testing.T, m *PluginManager, req string) string {
	t.Helper()
	input := jsonValue(map[string]any{"request": json.RawMessage(req)})
	b, e := m.Trigger(testCtx(), "reference", input)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}

// TestPluginConsentLifecycle cobre grants, settings, canais, embed e estado após reiniciar o runtime.
func TestPluginConsentLifecycle(t *testing.T) {
	m, u, ch := managerFixture(t, false)
	p, e := m.Preview(testCtx(), u.ID, "reference", PluginOptions{Settings: map[string]any{"target_channel": ch.ID}})
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Scopes) != 1 || p.Scopes[0] != ch.ID || len(p.Surfaces) != 2 {
		t.Fatalf("preview incompleto: %+v", p)
	}
	if _, e = m.Enable(testCtx(), u.ID, "reference", PluginOptions{Settings: p.Settings}); !errors.Is(e, ErrPluginConsent) {
		t.Fatal("enable sem consentimento", e)
	}
	a := enableReference(t, m, u, ch)
	if a.PluginState != "enabled" || a.AvatarMedia == nil {
		t.Fatalf("instalação %+v", a)
	}
	if ok, e := storage.AppHasScope(testCtx(), a.ID, ch.ID); e != nil || !ok {
		t.Fatal("setting channel não foi concedido", e)
	}
	var channel string
	if e = storage.GetDB().QueryRow("SELECT channel_id FROM app_channels WHERE app_installation_id=$1", a.ID).Scan(&channel); e != nil {
		t.Fatal(e)
	}
	out, e := m.Trigger(testCtx(), "reference", json.RawMessage(`{}`))
	if e != nil {
		t.Fatal(e)
	}
	var result struct {
		OK     bool                         `json:"ok"`
		Result models.MessageWithAttachment `json:"result"`
	}
	if json.Unmarshal(out, &result) != nil || !result.OK || len(result.Result.Embeds) != 1 || result.Result.Embeds[0].Surface == nil || result.Result.Embeds[0].Entry == "" {
		t.Fatalf("embed: %s", out)
	}
	if result.Result.Content == nil || !strings.Contains(*result.Result.Content, "asset.read") {
		t.Fatal("template não foi lido")
	}
	if v := pluginRequestTest(t, m, `{"method":"kv.put","key":"counter","value":42}`); !strings.Contains(v, `"revision":1`) {
		t.Fatal(v)
	}
	if v := pluginRequestTest(t, m, `{"method":"kv.put","key":"counter","value":43}`); !strings.Contains(v, `"revision":2`) {
		t.Fatal(v)
	}
	if v := pluginRequestTest(t, m, `{"method":"asset.read","path":"templates/message.txt","offset":4,"length":8}`); !strings.Contains(v, `"length":8`) {
		t.Fatal(v)
	}
	if v := pluginRequestTest(t, m, `{"method":"asset.read","path":"../outside","offset":0,"length":8}`); !strings.Contains(v, "invalid_param") {
		t.Fatal(v)
	}
	// Um runtime novo reencontra os metadados e o KV no banco sem on_enable na descoberta.
	if e = m.Host.Close(testCtx()); e != nil {
		t.Fatal(e)
	}
	h, e := wasmhost.New(testCtx(), wasmhost.DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close(testCtx())
	restarted, e := NewPluginManager(m.Config, h, PluginEffects{})
	if e != nil {
		t.Fatal(e)
	}
	SetPluginSurfaceSource(restarted)
	if e = restarted.Rescan(testCtx()); e != nil {
		t.Fatal(e)
	}
	if v := pluginRequestTest(t, restarted, `{"method":"kv.get","key":"counter"}`); !strings.Contains(v, `"value":43`) {
		t.Fatal(v)
	}
	if e = restarted.Disable(testCtx(), u.ID, "reference"); e != nil {
		t.Fatal(e)
	}
	var n int
	if e = storage.GetDB().QueryRow("SELECT count(*) FROM app_channels WHERE app_installation_id=$1", a.ID).Scan(&n); e != nil || n != 0 {
		t.Fatal("canais não removidos", n, e)
	}
	if _, e = storage.GetChannelByID(testCtx(), channel); e != storage.ErrNotFound {
		t.Fatal(e)
	}
	if ok, _ := storage.AppHasScope(testCtx(), a.ID, ch.ID); ok {
		t.Fatal("escopo sobreviveu disable")
	}
	items, err := ListMessageEmbeds(testCtx(), []string{result.Result.ID})
	if err != nil || len(items[result.Result.ID]) != 1 {
		t.Fatal(items, err)
	}
	embed := items[result.Result.ID][0]
	if embed.Entry != "" {
		t.Fatal("embed desativado ativo")
	}
	a2 := enableReference(t, restarted, u, ch)
	if a2.ID != a.ID {
		t.Fatal("re-enable trocou identidade")
	}
	if v := pluginRequestTest(t, restarted, `{"method":"kv.get","key":"counter"}`); !strings.Contains(v, `"value":43`) {
		t.Fatal(v)
	}
}

// TestPluginDigestAndConsent rejeita token adulterado, replay e edições de assets sem aprovação.
func TestPluginDigestAndConsent(t *testing.T) {
	m, u, ch := managerFixture(t, false)
	a := enableReference(t, m, u, ch)
	o := approvedOptions(t, m, u.ID, "reference", PluginOptions{Settings: map[string]any{"target_channel": ch.ID}})
	o.MaxMemoryMB = 8
	if _, e := m.Enable(testCtx(), u.ID, "reference", o); !errors.Is(e, ErrPluginConsent) {
		t.Fatal("token não vinculou memória", e)
	}
	name := filepath.Join(m.Config.Directory, "reference", "templates/message.txt")
	if e := os.WriteFile(name, []byte("alterado"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := m.Trigger(testCtx(), "reference", json.RawMessage(`{}`)); !errors.Is(e, ErrPluginConsent) {
		t.Fatal(e)
	}
	current, e := storage.GetApp(testCtx(), a.ID)
	if e != nil || current.PluginState != "quarantined" {
		t.Fatal(current.PluginState, e)
	}
	var n int
	_ = storage.GetDB().QueryRow("SELECT count(*) FROM app_channels WHERE app_installation_id=$1", a.ID).Scan(&n)
	if n != 1 {
		t.Fatal("quarentena apagou canal")
	}
	if _, e = m.Enable(testCtx(), u.ID, "reference", o); e == nil {
		t.Fatal("consentimento antigo aceito")
	}
	enableReference(t, m, u, ch)
	o = approvedOptions(t, m, u.ID, "reference", PluginOptions{Settings: map[string]any{"target_channel": ch.ID}})
	if _, e = m.Enable(testCtx(), u.ID, "reference", o); e != nil {
		t.Fatal(e)
	}
	if _, e = m.Enable(testCtx(), u.ID, "reference", o); !errors.Is(e, ErrPluginConsent) {
		t.Fatal("replay aceito", e)
	}
}

// TestPluginPatch exige reaprovação ao trocar o canal e conserva customizações da superfície.
func TestPluginPatch(t *testing.T) {
	m, u, ch := managerFixture(t, false)
	a := enableReference(t, m, u, ch)
	other := newWebhookTestChannel(t, "text")
	if _, e := m.Patch(testCtx(), u.ID, "reference", PluginOptions{MaxMemoryMB: 8}); e != nil {
		t.Fatal(e)
	}
	o := PluginOptions{Settings: map[string]any{"target_channel": other.ID}}
	if _, e := m.Patch(testCtx(), u.ID, "reference", o); !errors.Is(e, ErrPluginConsent) {
		t.Fatal("novo escopo sem consentimento", e)
	}
	if ok, _ := storage.AppHasScope(testCtx(), a.ID, other.ID); ok {
		t.Fatal("escopo concedido antes de consentimento")
	}
	o = approvedOptions(t, m, u.ID, "reference", o)
	if _, e := m.Patch(testCtx(), u.ID, "reference", o); e != nil {
		t.Fatal(e)
	}
	if ok, _ := storage.AppHasScope(testCtx(), a.ID, other.ID); !ok {
		t.Fatal("escopo setting ausente")
	}
	if ok, _ := storage.AppHasScope(testCtx(), a.ID, ch.ID); ok {
		t.Fatal("setting antigo preservou escopo")
	}
}

// TestPluginHotReload aplica assets sem expansão e bloqueia capacidades novas.
func TestPluginHotReload(t *testing.T) {
	m, u, ch := managerFixture(t, true)
	a := enableReference(t, m, u, ch)
	file := filepath.Join(m.Config.Directory, "reference", "templates/message.txt")
	if e := os.WriteFile(file, []byte("hot reload"), 0600); e != nil {
		t.Fatal(e)
	}
	if out, e := m.Trigger(testCtx(), "reference", json.RawMessage(`{}`)); e != nil || !strings.Contains(string(out), "hot reload") {
		t.Fatal(string(out), e)
	}
	current, _ := storage.GetApp(testCtx(), a.ID)
	if current.PluginDigest == nil || *current.PluginDigest == *a.PluginDigest {
		t.Fatal("digest não avançou")
	}
	manifest := filepath.Join(m.Config.Directory, "reference", "plugin.toml")
	b, e := os.ReadFile(manifest)
	if e != nil {
		t.Fatal(e)
	}
	b = []byte(strings.Replace(string(b), `"private_storage"`, `"private_storage", "read_messages"`, 1))
	if e = os.WriteFile(manifest, b, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = m.Trigger(testCtx(), "reference", json.RawMessage(`{}`)); !errors.Is(e, ErrPluginConsent) {
		t.Fatal("expansão automática", e)
	}
}

// TestPluginRemovalAndGlobalPolicy limpa canais de pastas removidas e nunca concede bloqueios globais.
func TestPluginRemovalAndGlobalPolicy(t *testing.T) {
	m, u, ch := managerFixture(t, false)
	m.Config.Blocked = []string{"private_storage"}
	p, e := m.Preview(testCtx(), u.ID, "reference", PluginOptions{Settings: map[string]any{"target_channel": ch.ID}})
	if e != nil {
		t.Fatal(e)
	}
	if has(p.Permissions, "private_storage") || !has(p.BlockedPermissions, "private_storage") {
		t.Fatal("bloqueio global ignorado")
	}
	a := enableReference(t, m, u, ch)
	if v := pluginRequestTest(t, m, `{"method":"kv.put","key":"counter","value":42}`); !strings.Contains(v, "forbidden") {
		t.Fatal(v)
	}
	if e = os.RemoveAll(filepath.Join(m.Config.Directory, "reference")); e != nil {
		t.Fatal(e)
	}
	if e = m.Rescan(testCtx()); e != nil {
		t.Fatal(e)
	}
	a, e = storage.GetApp(testCtx(), a.ID)
	if e != nil || a.PluginState != "removed" || a.Capabilities.SendMessages {
		t.Fatal(a, e)
	}
}

// TestPluginHostAuthorization usa checks iguais aos do REST, sem identidade fornecida pelo plugin.
func TestPluginHostAuthorization(t *testing.T) {
	m, u, ch := managerFixture(t, false)
	a := enableReference(t, m, u, ch)
	other := newWebhookTestChannel(t, "text")
	req := `{"method":"message.send","channel_id":"` + other.ID + `","content":"denied"}`
	if v := pluginRequestTest(t, m, req); !strings.Contains(v, "forbidden") {
		t.Fatal(v)
	}
	caps := models.AppCapabilities{PrivateStorage: true}
	if _, e := UpdateApp(testCtx(), u.ID, a.ID, nil, &caps, nil, nil, ""); e != nil {
		t.Fatal(e)
	}
	req = `{"method":"message.send","channel_id":"` + ch.ID + `","content":"denied"}`
	if v := pluginRequestTest(t, m, req); !strings.Contains(v, "forbidden") {
		t.Fatal(v)
	}
	req = `{"method":"message.send","channel_id":"` + ch.ID + `","embeds":[{"source_type":"app","app_surface_id":"other"}]}`
	if v := pluginRequestTest(t, m, req); !strings.Contains(v, `"ok":false`) {
		t.Fatal(v)
	}
}

// faultyLifecycle executa o fixture de trap pelo runtime real somente no callback escolhido.
type faultyLifecycle struct {
	plugins.PluginHost
	event string
	evil  []byte
	calls int
}

// Invoke permite confirmar que descoberta/check nunca executam callbacks.
func (h *faultyLifecycle) Invoke(ctx context.Context, id string, artifact, input []byte, call plugins.Call) ([]byte, error) {
	h.calls++
	var ev struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(input, &ev)
	if ev.Type == h.event {
		return h.PluginHost.Invoke(ctx, id, h.evil, []byte(`{"op":"trap"}`), call)
	}
	return h.PluginHost.Invoke(ctx, id, artifact, input, call)
}

// TestPluginQuarantine persiste o circuito após três traps e mantém o canal indisponível.
func TestPluginQuarantine(t *testing.T) {
	m, u, ch := managerFixture(t, false)
	evil, e := os.ReadFile("../wasmhost/testdata/adversarial.wasm")
	if e != nil {
		t.Fatal(e)
	}
	host := &faultyLifecycle{PluginHost: m.Host, event: "on_trigger", evil: evil}
	m.Host = host
	if e = m.Rescan(testCtx()); e != nil {
		t.Fatal(e)
	}
	if host.calls != 0 {
		t.Fatal("discovery executou código")
	}
	a := enableReference(t, m, u, ch)
	for i := 0; i < 3; i++ {
		if _, e = m.Trigger(testCtx(), "reference", json.RawMessage(`{}`)); e == nil {
			t.Fatal("trap ausente")
		}
	}
	a, e = storage.GetApp(testCtx(), a.ID)
	if e != nil || a.PluginState != "quarantined" {
		t.Fatal("circuito não persistiu", a.PluginState, e)
	}
	var n int
	_ = storage.GetDB().QueryRow("SELECT count(*) FROM app_channels WHERE app_installation_id=$1", a.ID).Scan(&n)
	if n != 1 {
		t.Fatal("circuito apagou canal")
	}
	host.event = "on_disable"
	enableReference(t, m, u, ch)
	if e = m.Disable(testCtx(), u.ID, "reference"); e == nil {
		t.Fatal("on_disable deveria falhar")
	}
	a, e = storage.GetApp(testCtx(), a.ID)
	if e != nil || a.PluginState != "disabled" || a.Capabilities.SendMessages {
		t.Fatal("falha manteve grants", a, e)
	}
	_ = storage.GetDB().QueryRow("SELECT count(*) FROM app_channels WHERE app_installation_id=$1", a.ID).Scan(&n)
	if n != 0 {
		t.Fatal("disable falho manteve canais")
	}
}

// TestPluginPendingTransition falha fechado no startup após término abrupto entre etapas.
func TestPluginPendingTransition(t *testing.T) {
	m, u, ch := managerFixture(t, false)
	a := enableReference(t, m, u, ch)
	if _, e := storage.GetDB().Exec("UPDATE app_installations SET lifecycle_pending=true WHERE id=$1", a.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := m.Resolve(testCtx(), a.ID); e == nil {
		t.Fatal("UI de transição publicada")
	}
	if _, e := m.Trigger(testCtx(), "reference", json.RawMessage(`{}`)); e == nil {
		t.Fatal("trigger de transição aceito")
	}
	if e := m.Rescan(testCtx()); e != nil {
		t.Fatal(e)
	}
	a, e := storage.GetApp(testCtx(), a.ID)
	if e != nil || a.PluginState != "quarantined" || a.LifecyclePending {
		t.Fatal(a, e)
	}
}

// TestPluginOriginExpansion exige consentimento mesmo com hot reload habilitado.
func TestPluginOriginExpansion(t *testing.T) {
	m, u, ch := managerFixture(t, true)
	enableReference(t, m, u, ch)
	file := filepath.Join(m.Config.Directory, "reference", "plugin.toml")
	b, e := os.ReadFile(file)
	if e != nil {
		t.Fatal(e)
	}
	b = append([]byte("remote_origins = [\"https://8.8.8.8\"]\n"), b...)
	if e = os.WriteFile(file, b, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = m.Trigger(testCtx(), "reference", json.RawMessage(`{}`)); !errors.Is(e, ErrPluginConsent) {
		t.Fatal("origem nova aplicada sem consentimento", e)
	}
}

// TestPluginQuotaConcurrent mantém o teto sob duas escritas concorrentes na última vaga.
func TestPluginQuotaConcurrent(t *testing.T) {
	m, u, ch := managerFixture(t, false)
	a := enableReference(t, m, u, ch)
	if _, e := storage.GetDB().Exec("DELETE FROM app_storage WHERE installation=$1", a.ID); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 255; i++ {
		key := fmt.Sprint(i)
		if _, e := storage.MutateAppStorage(testCtx(), a.ID, key, json.RawMessage(`0`), false); e != nil {
			t.Fatal(e)
		}
	}
	results := make(chan error, 2)
	for _, key := range []string{"last-a", "last-b"} {
		go func(k string) {
			_, e := storage.MutateAppStorage(testCtx(), a.ID, k, json.RawMessage(`0`), false)
			results <- e
		}(key)
	}
	success, quota := 0, 0
	for i := 0; i < 2; i++ {
		e := <-results
		if e == nil {
			success++
		} else if errors.Is(e, storage.ErrAppStorageQuota) {
			quota++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || quota != 1 {
		t.Fatal("quota concorrente violada", success, quota)
	}
}

// Inline-only installations must notify readers even without an app channel.
func TestPluginInvalidationWithoutAppChannel(t *testing.T) {
	m, u, ch := managerFixture(t, false)
	a := enableReference(t, m, u, ch)
	if _, err := m.Trigger(testCtx(), "reference", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := DeleteAppChannels(testCtx(), u.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := storage.AppSurfaceChannelIDs(testCtx(), a.ID)
	found := false
	for _, id := range ids {
		if id == ch.ID {
			found = true
		}
	}
	if err != nil || !found {
		t.Fatalf("inline-only audience: %v %v", ids, err)
	}
	var states []string
	m.Effects.Invalidate = func(ctx context.Context, id string) {
		if id != a.ID {
			t.Errorf("wrong installation %s", id)
		}
		current, err := storage.GetApp(ctx, id)
		if err != nil {
			t.Error(err)
			return
		}
		states = append(states, current.PluginState)
	}
	if err := m.quarantine(testCtx(), a); err != nil {
		t.Fatal(err)
	}
	if err := m.Disable(testCtx(), u.ID, "reference"); err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || states[0] != "quarantined" || states[1] != "disabled" {
		t.Fatalf("notifications after authority closes: %v", states)
	}
}
