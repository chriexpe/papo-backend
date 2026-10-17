package services

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"papo/internal/models"
	"papo/internal/plugins"
	"papo/internal/storage"
	"strings"
	"testing"
)

// profileHost observa o envelope real sem substituir a execução TinyGo.
type profileHost struct {
	plugins.PluginHost
	inputs map[string]json.RawMessage
}

func (h *profileHost) Invoke(ctx context.Context, id string, b, input []byte, call plugins.Call) ([]byte, error) {
	var event struct {
		Type  string          `json:"type"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(input, &event) == nil {
		h.inputs[event.Type] = append([]byte(nil), event.Input...)
	}
	return h.PluginHost.Invoke(ctx, id, b, input, call)
}
func assertPublicProfile(t *testing.T, b json.RawMessage, user models.User) {
	t.Helper()
	var p map[string]any
	if json.Unmarshal(b, &p) != nil || len(p) != 4 || p["id"] != user.ID || p["username"] != user.Username || p["avatar_url"] != "/users/"+user.ID+"/avatar" {
		t.Fatalf("perfil público inválido: %s", b)
	}
	want := user.Username
	if user.Nickname != nil && *user.Nickname != "" {
		want = *user.Nickname
	}
	if p["display_name"] != want {
		t.Fatal(p)
	}
}

// TestAppCallbackProfiles garante identidade e projeção mínima nos dois callbacks humanos.
func TestAppCallbackProfiles(t *testing.T) {
	m, u, v, a, channel, _ := interactionFixture(t)
	name := "Jogadora verificada"
	v.Nickname = &name
	if _, e := storage.GetDB().Exec(`UPDATE users SET nickname=$2 WHERE id=$1`, v.ID, name); e != nil {
		t.Fatal(e)
	}
	spy := &profileHost{PluginHost: m.Host, inputs: map[string]json.RawMessage{}}
	m.Host = spy
	if _, e := m.Interact(testCtx(), u.ID, InteractionInput{InstallationID: a.ID, Kind: "user_action", ChannelID: channel, ActionID: "convidar", TargetUserID: v.ID}); e != nil {
		t.Fatal(e)
	}
	var input map[string]json.RawMessage
	json.Unmarshal(spy.inputs["on_interaction"], &input)
	assertPublicProfile(t, input["user"], u)
	assertPublicProfile(t, input["target_user"], v)
	token := launchSession(t, m, v.ID, a.ID, channel)
	if _, e := m.AppRPC(testCtx(), a.ID, token, "identity", json.RawMessage(`{"user":{"id":"forjado"}}`)); e != nil {
		t.Fatal(e)
	}
	json.Unmarshal(spy.inputs["on_ui_call"], &input)
	assertPublicProfile(t, input["user"], v)
}

// TestAppUsersResolve aplica a união de canais próprios/escopos atuais, sem distinguir ausentes de negados.
func TestAppUsersResolve(t *testing.T) {
	m, u, v, a, channel, text := interactionFixture(t)
	role, e := storage.CreateRole(testCtx(), "profiles_"+uuid.NewString(), nil, models.RolePermissions{})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = storage.AssignUserRole(testCtx(), u.ID, role.ID); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{channel, text.ID} {
		if _, e = storage.UpdateChannelPermissions(testCtx(), id, role.ID, models.ChannelPermission{ReadChannel: true}); e != nil {
			t.Fatal(e)
		}
	}
	profiles, e := m.ResolveUsers(testCtx(), a.ID, []string{v.ID, uuid.NewString(), u.ID, u.ID})
	if e != nil || len(profiles) != 1 {
		t.Fatal(profiles, e)
	}
	b, _ := json.Marshal(profiles[0])
	assertPublicProfile(t, b, u)
	// Outro canal público não concede visibilidade ao plugin.
	newWebhookTestChannel(t, "text")
	denied, e := m.ResolveUsers(testCtx(), a.ID, []string{v.ID})
	if e != nil || len(denied) != 0 {
		t.Fatal(denied, e)
	}
	missing, e := m.ResolveUsers(testCtx(), a.ID, []string{uuid.NewString()})
	if e != nil || len(missing) != 0 {
		t.Fatal(missing, e)
	}
	if _, e = storage.AssignUserRole(testCtx(), v.ID, role.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = storage.UpdateChannelPermissions(testCtx(), channel, role.ID, models.ChannelPermission{}); e != nil {
		t.Fatal(e)
	}
	profiles, e = m.ResolveUsers(testCtx(), a.ID, []string{v.ID})
	if e != nil || len(profiles) != 1 {
		t.Fatal("escopo", profiles, e)
	}
	if e = storage.RemoveAppScope(testCtx(), a.ID, text.ID); e != nil {
		t.Fatal(e)
	}
	profiles, e = m.ResolveUsers(testCtx(), a.ID, []string{v.ID})
	if e != nil || len(profiles) != 0 {
		t.Fatal("escopo retirado", profiles, e)
	}
	ids := make([]string, 50)
	for i := range ids {
		ids[i] = u.ID
	}
	if _, e = m.ResolveUsers(testCtx(), a.ID, ids); e != nil {
		t.Fatal("limite exato", e)
	}
	if _, e = m.ResolveUsers(testCtx(), a.ID, append(ids, u.ID)); !errors.Is(e, ErrInvalidInput) {
		t.Fatal("mais de 50", e)
	}
	m.limits = newAppLimiter()
	m.Config.Limits.UsersResolvePerPlugin = 1
	if _, e = m.ResolveUsers(testCtx(), a.ID, []string{u.ID}); e != nil {
		t.Fatal(e)
	}
	if _, e = m.ResolveUsers(testCtx(), a.ID, []string{u.ID}); !errors.Is(e, ErrAppRateLimit) {
		t.Fatal("rate limit", e)
	}
}

// TestAppStorageQuotaOverrides percorre PATCH administrativo, herança e reset dos limites no host real.
func TestAppStorageQuotaOverrides(t *testing.T) {
	m, u, _, a, _, _ := interactionFixture(t)
	keys, total, value := 2, 64, 12
	quota := models.AppStorageQuotaOverride{MaxKeys: &keys, MaxTotalBytes: &total, MaxValueBytes: &value}
	updated, e := m.Patch(testCtx(), u.ID, "reference", PluginOptions{StorageQuota: &quota})
	if e != nil {
		t.Fatal(e)
	}
	if updated.StorageQuota.MaxKeys != 2 || updated.StorageQuota.MaxTotalBytes != 64 || updated.StorageQuota.MaxValueBytes != 12 {
		t.Fatal(updated.StorageQuota)
	}
	if _, e = storage.GetDB().Exec(`DELETE FROM app_storage WHERE installation=$1`, a.ID); e != nil {
		t.Fatal(e)
	}
	put := func(key, raw string) error {
		_, e := storage.MutateAppStorage(testCtx(), a.ID, key, json.RawMessage(raw), false)
		return e
	}
	if e = put("a", `"ok"`); e != nil {
		t.Fatal(e)
	}
	if e = put("b", `"ok"`); e != nil {
		t.Fatal(e)
	}
	if e = put("c", `1`); !errors.Is(e, storage.ErrAppStorageQuota) {
		t.Fatal("chaves", e)
	}
	if e = put("a", `"muito maior que o limite"`); !errors.Is(e, storage.ErrAppStorageQuota) {
		t.Fatal("valor", e)
	}
	bigger := 32
	if _, e = m.Patch(testCtx(), u.ID, "reference", PluginOptions{StorageQuota: &models.AppStorageQuotaOverride{MaxValueBytes: &bigger}}); e != nil {
		t.Fatal(e)
	}
	if e = put("a", `"valor maior que dezesseis"`); e != nil {
		t.Fatal("override não aumentou valor", e)
	}
	reset := 0
	restored, e := m.Patch(testCtx(), u.ID, "reference", PluginOptions{StorageQuota: &models.AppStorageQuotaOverride{MaxKeys: &reset, MaxTotalBytes: &reset, MaxValueBytes: &reset}})
	if e != nil || restored.StorageQuota.MaxKeys != 256 || restored.StorageQuota.MaxTotalBytes != 1<<20 {
		t.Fatal(restored, e)
	}
}

// TestAppStorageEnvAndLargeHostValue evita o antigo teto fixo de 16 KiB no host.
func TestAppStorageEnvAndLargeHostValue(t *testing.T) {
	m, u, _, a, _, _ := interactionFixture(t)
	t.Cleanup(func() { storage.GetDB().Exec(`UPDATE app_installations SET storage_quota='{}' WHERE id=$1`, a.ID) })
	t.Setenv("PLUGINS_STORAGE_MAX_KEYS", "1")
	t.Setenv("PLUGINS_STORAGE_MAX_TOTAL_BYTES", "65536")
	t.Setenv("PLUGINS_STORAGE_MAX_VALUE_BYTES", "32768")
	if _, e := storage.GetDB().Exec(`DELETE FROM app_storage WHERE installation=$1`, a.ID); e != nil {
		t.Fatal(e)
	}
	large := strings.Repeat("x", 20000)
	request := string(jsonValue(map[string]any{"method": "kv.put", "key": "large", "value": large}))
	out := pluginRequestTest(t, m, request)
	var envelope struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal([]byte(out), &envelope) != nil || !envelope.OK {
		t.Fatal("host ignorou quota ampliada", out)
	}
	if _, e := storage.MutateAppStorage(testCtx(), a.ID, "other", json.RawMessage(`1`), false); !errors.Is(e, storage.ErrAppStorageQuota) {
		t.Fatal("default global de chaves", e)
	}
	// Override de chave prevalece sobre o env, mas os demais limites continuam herdados.
	n := 2
	updated, e := m.Patch(testCtx(), u.ID, "reference", PluginOptions{StorageQuota: &models.AppStorageQuotaOverride{MaxKeys: &n}})
	if e != nil || updated.StorageQuota.MaxKeys != 2 || updated.StorageQuota.MaxValueBytes != 32768 {
		t.Fatal(updated, e)
	}
	if _, e = storage.MutateAppStorage(testCtx(), a.ID, "other", json.RawMessage(`1`), false); e != nil {
		t.Fatal(e)
	}
}
