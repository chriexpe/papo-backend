package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"papo/internal/models"
	"papo/internal/plugins"
	"papo/internal/storage"
)

// interactionFixture habilita o módulo real e cria dois leitores sem privilégios de gestão.
func interactionFixture(t *testing.T) (*PluginManager, models.User, models.User, models.AppInstallation, string, models.Channel) {
	t.Helper()
	m, u, ch := managerFixture(t, false)
	v := newWebhookTestUser(t)
	a := enableReference(t, m, u, ch)
	var appChannel string
	if e := storage.GetDB().QueryRow(`SELECT channel_id FROM app_channels WHERE app_installation_id=$1`, a.ID).Scan(&appChannel); e != nil {
		t.Fatal(e)
	}
	if _, e := storage.GetDB().Exec(`UPDATE channels SET permissions='{}' WHERE id=$1`, appChannel); e != nil {
		t.Fatal(e)
	}
	return m, u, v, a, appChannel, ch
}
func launchSession(t *testing.T, m *PluginManager, user, id, channel string) string {
	t.Helper()
	ticket, e := m.LaunchAppUI(testCtx(), user, id, LaunchInput{SurfaceID: "home", ChannelID: channel})
	if e != nil {
		t.Fatal(e)
	}
	session, e := m.ExchangeAppTicket(testCtx(), id, ticket["ticket"].(string))
	if e != nil {
		t.Fatal(e)
	}
	return session["token"].(string)
}
func denyAppRead(t *testing.T, channel string) {
	t.Helper()
	if _, e := storage.GetDB().Exec(`UPDATE channels SET permissions=$2 WHERE id=$1`, channel, string(jsonValue(map[string]any{uuid.NewString(): models.ChannelPermission{}}))); e != nil {
		t.Fatal(e)
	}
}

// TestAppInteractionsGame percorre convite humano, aceitação, RPC verificado e edição de botões.
func TestAppInteractionsGame(t *testing.T) {
	m, u, v, a, channel, text := interactionFixture(t)
	in := InteractionInput{InstallationID: a.ID, Kind: "user_action", ChannelID: channel, ActionID: "convidar", TargetUserID: v.ID}
	if _, e := m.Interact(testCtx(), u.ID, in); e != nil {
		t.Fatal(e)
	}
	list, e := ListUserNotifications(testCtx(), v.ID, v.ID, nil, "")
	if e != nil {
		t.Fatal(e)
	}
	if len(list.Notifications) != 1 || list.Notifications[0].Kind != "app_invite" {
		t.Fatalf("convite: %+v", list)
	}
	invite := list.Notifications[0].ID
	if _, e = m.DecideInvite(testCtx(), u.ID, invite, "accepted"); e == nil {
		t.Fatal("remetente aceitou pelo alvo")
	}
	if _, e = m.DecideInvite(testCtx(), v.ID, invite, "accepted"); e != nil {
		t.Fatal(e)
	}
	if _, e = m.DecideInvite(testCtx(), v.ID, invite, "accepted"); e != nil {
		t.Fatal("repetição", e)
	}
	if _, e = m.DecideInvite(testCtx(), v.ID, invite, "declined"); e == nil {
		t.Fatal("mudou decisão")
	}
	ticket, e := m.LaunchAppUI(testCtx(), v.ID, a.ID, LaunchInput{SurfaceID: "home", ChannelID: channel, InviteID: invite})
	if e != nil {
		t.Fatal(e)
	}
	session, e := m.ExchangeAppTicket(testCtx(), a.ID, ticket["ticket"].(string))
	if e != nil {
		t.Fatal(e)
	}
	targetToken := session["token"].(string)
	senderToken := launchSession(t, m, u.ID, a.ID, channel)
	output, e := m.AppRPC(testCtx(), a.ID, targetToken, "identity", json.RawMessage(`{}`))
	if e != nil || !strings.Contains(string(output), v.ID) || strings.Contains(string(output), u.ID) {
		t.Fatal("identidade não verificada", string(output), e)
	}
	if _, e = m.AppRPC(testCtx(), a.ID, senderToken, "game.move", json.RawMessage(`{"cell":0,"revision":0}`)); e != nil {
		t.Fatal(e)
	}
	output, e = m.AppRPC(testCtx(), a.ID, targetToken, "game.state", json.RawMessage(`{}`))
	if e != nil || !strings.Contains(string(output), `"X"`) {
		t.Fatal(string(output), e)
	}
	if _, e = m.AppRPC(testCtx(), a.ID, senderToken, "game.move", json.RawMessage(`{"cell":1,"revision":1}`)); e == nil {
		t.Fatal("jogou fora de turno")
	}
	if _, e = m.AppRPC(testCtx(), a.ID, targetToken, "game.move", json.RawMessage(`{"cell":1,"revision":1}`)); e != nil {
		t.Fatal(e)
	}
	messages, e := ListMessages(testCtx(), text.ID, u.ID, nil, "")
	if e != nil || len(messages.Messages) != 1 {
		t.Fatal(messages, e)
	}
	msg := messages.Messages[0]
	if len(msg.Components) != 1 || len(msg.Components[0]) != 2 {
		t.Fatal("components ausentes", msg)
	}
	if _, _, e = storage.PinMessage(testCtx(), text.ID, msg.ID, u.ID); e != nil {
		t.Fatal(e)
	}
	pinned, e := ListPinnedMessages(testCtx(), text.ID, u.ID)
	if e != nil || len(pinned.Pinned) != 1 || len(pinned.Pinned[0].Components) != 1 {
		t.Fatal("fixados", pinned, e)
	}
	if _, e = m.LaunchAppUI(testCtx(), u.ID, a.ID, LaunchInput{SurfaceID: "result", ChannelID: text.ID, MessageID: msg.ID}); e != nil {
		t.Fatal("opens", e)
	}
	edited := false
	m.Effects.Edited = func(_ context.Context, msg models.MessageWithAttachment, _ string) { edited = len(msg.Components) == 1 }
	if _, e = m.Interact(testCtx(), u.ID, InteractionInput{InstallationID: a.ID, Kind: "button", ChannelID: text.ID, MessageID: msg.ID, ComponentID: "revanche"}); e != nil || !edited {
		t.Fatal("revanche não editou componentes", e)
	}
	if e = m.Disable(testCtx(), u.ID, "reference"); e != nil {
		t.Fatal(e)
	}
	rows, e := storage.AppComponents(testCtx(), []string{msg.ID})
	if e != nil || !rows[msg.ID][0][0].Disabled {
		t.Fatal("botões ativos após disable", e)
	}
	if _, e = m.AppRPC(testCtx(), a.ID, senderToken, "identity", json.RawMessage(`{}`)); !errors.Is(e, ErrAppSession) {
		t.Fatal("sessão sobreviveu disable", e)
	}
	if _, e = m.Interact(testCtx(), u.ID, in); e == nil {
		t.Fatal("interação após disable")
	}
}

// TestAppInteractionDenials impede seletores forjados, spam e convites autônomos.
func TestAppInteractionDenials(t *testing.T) {
	m, u, v, a, channel, _ := interactionFixture(t)
	for _, in := range []InteractionInput{{InstallationID: a.ID, Kind: "user_action", ChannelID: channel, ActionID: "ausente", TargetUserID: v.ID}, {InstallationID: a.ID, Kind: "button", ChannelID: channel, MessageID: uuid.NewString(), ComponentID: "ausente"}} {
		if _, e := m.Interact(testCtx(), u.ID, in); e == nil {
			t.Fatal("seletor aceito")
		}
	}
	if _, e := m.SendAppInvite(testCtx(), a, v.ID, "Jogar", json.RawMessage(`{}`)); !errors.Is(e, ErrPermissionDenied) {
		t.Fatal("convite sem humano", e)
	}
	raw := pluginRequestTest(t, m, `{"method":"invite.send","to_user_id":"`+v.ID+`","text":"Jogar","data":{}}`)
	if !strings.Contains(raw, `"ok":false`) {
		t.Fatal("trigger convidou", raw)
	}
	in := InteractionInput{InstallationID: a.ID, Kind: "user_action", ChannelID: channel, ActionID: "convidar", TargetUserID: v.ID}
	t.Setenv("PLUGINS_INTERACTIONS_PER_USER", "1")
	if _, e := m.Interact(testCtx(), u.ID, in); e != nil {
		t.Fatal(e)
	}
	if _, e := m.Interact(testCtx(), u.ID, in); !errors.Is(e, ErrAppRateLimit) {
		t.Fatal("limite usuário", e)
	}
	denyAppRead(t, channel)
	if _, e := m.Interact(testCtx(), u.ID, in); e == nil {
		t.Fatal("interação sem leitura")
	}
	ctx := context.WithValue(testCtx(), appHumanKey{}, appHuman{u.ID, channel})
	if _, e := m.SendAppInvite(ctx, a, v.ID, "Jogar", json.RawMessage(`{}`)); e == nil {
		t.Fatal("convite sem leitura")
	}
}

// TestAppUITokenBoundaries cobre replay, expiração, inatividade, prazo absoluto e revogação.
func TestAppUITokenBoundaries(t *testing.T) {
	m, u, _, a, channel, _ := interactionFixture(t)
	issue := func() string {
		r, e := m.LaunchAppUI(testCtx(), u.ID, a.ID, LaunchInput{SurfaceID: "home", ChannelID: channel})
		if e != nil {
			t.Fatal(e)
		}
		return r["ticket"].(string)
	}
	ticket := issue()
	if _, e := m.ExchangeAppTicket(testCtx(), a.ID, ticket); e != nil {
		t.Fatal(e)
	}
	if _, e := m.ExchangeAppTicket(testCtx(), a.ID, ticket); !errors.Is(e, ErrAppSession) {
		t.Fatal("ticket reutilizado", e)
	}
	ticket = issue()
	if _, e := storage.GetDB().Exec(`UPDATE app_ui_tokens SET expires_at=now()-interval '1 second' WHERE token_hash=$1`, appHash(ticket)); e != nil {
		t.Fatal(e)
	}
	if _, e := m.ExchangeAppTicket(testCtx(), a.ID, ticket); !errors.Is(e, ErrAppSession) {
		t.Fatal("ticket expirado", e)
	}
	token := launchSession(t, m, u.ID, a.ID, channel)
	if _, e := storage.GetDB().Exec(`UPDATE app_ui_tokens SET expires_at=now()+interval '5 seconds',absolute_expires_at=now()+interval '10 minutes' WHERE token_hash=$1`, appHash(token)); e != nil {
		t.Fatal(e)
	}
	if _, e := m.AppRPC(testCtx(), a.ID, token, "identity", json.RawMessage(`{}`)); e != nil {
		t.Fatal(e)
	}
	saved, e := storage.GetAppUIToken(testCtx(), appHash(token), "session", a.ID)
	if e != nil || time.Until(saved.ExpiresAt) < 9*time.Minute || saved.ExpiresAt.After(saved.AbsoluteExpiresAt) {
		t.Fatal("expiração deslizante", saved, e)
	}
	if _, e = storage.GetDB().Exec(`UPDATE app_ui_tokens SET absolute_expires_at=now()-interval '1 second' WHERE token_hash=$1`, appHash(token)); e != nil {
		t.Fatal(e)
	}
	if _, e = m.AppRPC(testCtx(), a.ID, token, "identity", json.RawMessage(`{}`)); !errors.Is(e, ErrAppSession) {
		t.Fatal("limite absoluto", e)
	}
	token = launchSession(t, m, u.ID, a.ID, channel)
	m.invalidate(testCtx(), a.ID)
	if _, e = m.AppRPC(testCtx(), a.ID, token, "identity", json.RawMessage(`{}`)); !errors.Is(e, ErrAppSession) {
		t.Fatal("app_invalidate", e)
	}
	token = launchSession(t, m, u.ID, a.ID, channel)
	denyAppRead(t, channel)
	if _, e = m.AppRPC(testCtx(), a.ID, token, "identity", json.RawMessage(`{}`)); !errors.Is(e, ErrAppSession) {
		t.Fatal("perda de acesso", e)
	}
	if _, e = storage.GetDB().Exec(`UPDATE channels SET permissions='{}' WHERE id=$1`, channel); e != nil {
		t.Fatal(e)
	}
	if _, e = m.AppRPC(testCtx(), a.ID, token, "identity", json.RawMessage(`{}`)); !errors.Is(e, ErrAppSession) {
		t.Fatal("token ressuscitou", e)
	}
	if e = storage.CleanAppInteractions(testCtx()); e != nil {
		t.Fatal(e)
	}
	stop := StartAppInteractionCleanup(testCtx())
	stop()
}

// TestAppInviteLimitsMuteAndUICall garante os mesmos limites nas duas origens humanas.
func TestAppInviteLimitsMuteAndUICall(t *testing.T) {
	m, u, v, a, channel, _ := interactionFixture(t)
	ctx := context.WithValue(testCtx(), appHumanKey{}, appHuman{u.ID, channel})
	t.Setenv("PLUGINS_INVITES_PER_SENDER", "1")
	if _, e := m.SendAppInvite(ctx, a, v.ID, "Jogar", json.RawMessage(`{}`)); e != nil {
		t.Fatal(e)
	}
	if _, e := m.SendAppInvite(ctx, a, v.ID, "Jogar", json.RawMessage(`{}`)); !errors.Is(e, ErrAppRateLimit) {
		t.Fatal("limite remetente", e)
	}
	t.Setenv("PLUGINS_INVITES_PER_SENDER", "100")
	t.Setenv("PLUGINS_INVITES_PER_TARGET", "1")
	if _, e := m.SendAppInvite(ctx, a, v.ID, "Jogar", json.RawMessage(`{}`)); !errors.Is(e, ErrAppRateLimit) {
		t.Fatal("limite alvo", e)
	}
	t.Setenv("PLUGINS_INVITES_PER_TARGET", "100")
	if e := SetPluginInviteMute(testCtx(), v.ID, a.ID, true); e != nil {
		t.Fatal(e)
	}
	if _, e := m.SendAppInvite(ctx, a, v.ID, "Jogar", json.RawMessage(`{}`)); e == nil {
		t.Fatal("mute ignorado")
	}
	if e := SetPluginInviteMute(testCtx(), v.ID, a.ID, false); e != nil {
		t.Fatal(e)
	}
	token := launchSession(t, m, u.ID, a.ID, channel)
	if _, e := m.AppRPC(testCtx(), a.ID, token, "game.invite", jsonValue(map[string]string{"target_user_id": v.ID})); e != nil {
		t.Fatal("on_ui_call não convidou", e)
	}
	t.Setenv("PLUGINS_RPC_PER_SESSION", "1")
	if _, e := m.AppRPC(testCtx(), a.ID, token, "identity", json.RawMessage(`{}`)); !errors.Is(e, ErrAppRateLimit) {
		t.Fatal("limite RPC", e)
	}
}

// stalledCallback conserva o runtime real para provar que o prazo também cobre a espera do callback.
type stalledCallback struct{ plugins.PluginHost }

func (h stalledCallback) Invoke(ctx context.Context, _ string, _ []byte, _ []byte, _ plugins.Call) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestAppCallbackTimeout mantém o processo saudável depois de um callback que não termina.
func TestAppCallbackTimeout(t *testing.T) {
	m, u, v, a, channel, _ := interactionFixture(t)
	t.Setenv("PLUGINS_INTERACTION_TIMEOUT_MS", "20")
	real := m.Host
	m.Host = stalledCallback{real}
	_, e := m.Interact(testCtx(), u.ID, InteractionInput{InstallationID: a.ID, ChannelID: channel, Kind: "user_action", ActionID: "convidar", TargetUserID: v.ID})
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	m.Host = real
	if _, e = m.Trigger(testCtx(), "reference", json.RawMessage(`{}`)); e != nil {
		t.Fatal("runtime afetado", e)
	}
}

// TestAppButtonValidation cobre contagem, linhas, duplicação e opens de outra superfície.
func TestAppButtonValidation(t *testing.T) {
	m, u, _, a, _, _ := interactionFixture(t)
	_ = m
	_ = u
	valid := models.AppButton{ID: "a", Label: "Jogar", Style: "primary", Opens: "result"}
	for _, rows := range [][][]models.AppButton{{{valid, valid}}, {{{ID: "a", Label: "A", Style: "disabled"}}}, {{{ID: "a", Label: "A", Style: "primary", Opens: "home"}}}, {{valid}, {valid}, {valid}}, {{valid, valid, valid, valid, valid, valid}}} {
		if e := ValidateAppButtons(testCtx(), a.ID, rows); e == nil {
			t.Fatal("botão inválido aceito", rows)
		}
	}
	if e := ValidateAppButtons(testCtx(), a.ID, [][]models.AppButton{{valid}}); e != nil {
		t.Fatal(e)
	}
}

// TestAppInviteTargetAndCapabilities impede alvo sem acesso e capacidade retirada.
func TestAppInviteTargetAndCapabilities(t *testing.T) {
	m, u, v, a, channel, _ := interactionFixture(t)
	role, e := storage.CreateRole(testCtx(), "app_acl_"+randHex(4), nil, models.RolePermissions{})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = storage.AssignUserRole(testCtx(), u.ID, role.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = storage.UpdateChannelPermissions(testCtx(), channel, role.ID, models.ChannelPermission{ReadChannel: true}); e != nil {
		t.Fatal(e)
	}
	if e = CanReadChannel(testCtx(), channel, u.ID); e != nil {
		t.Fatal(e)
	}
	if e = CanReadChannel(testCtx(), channel, v.ID); e == nil {
		t.Fatal("alvo não deveria ler")
	}
	ctx := context.WithValue(testCtx(), appHumanKey{}, appHuman{u.ID, channel})
	if _, e = m.SendAppInvite(ctx, a, v.ID, "Jogar", json.RawMessage(`{}`)); !errors.Is(e, ErrPermissionDenied) {
		t.Fatal("alvo sem acesso", e)
	}
	a.Capabilities.SendInvites = false
	if _, e = m.SendAppInvite(ctx, a, v.ID, "Jogar", json.RawMessage(`{}`)); !errors.Is(e, ErrPermissionDenied) {
		t.Fatal("capacidade negada", e)
	}
}

// TestAppInstallationRateLimits confirma limites distintos por instalação e por usuário.
func TestAppInstallationRateLimits(t *testing.T) {
	m, u, v, a, channel, _ := interactionFixture(t)
	t.Setenv("PLUGINS_INTERACTIONS_PER_PLUGIN", "1")
	if _, e := storage.GetDB().Exec(`DELETE FROM app_rate_limits WHERE key=$1`, "interaction:plugin:"+a.ID); e != nil {
		t.Fatal(e)
	}
	in := InteractionInput{InstallationID: a.ID, Kind: "user_action", ChannelID: channel, ActionID: "convidar", TargetUserID: v.ID}
	if _, e := m.Interact(testCtx(), u.ID, in); e != nil {
		t.Fatal(e)
	}
	if _, e := m.Interact(testCtx(), u.ID, in); !errors.Is(e, ErrAppRateLimit) {
		t.Fatal("limite plugin", e)
	}
	token := launchSession(t, m, u.ID, a.ID, channel)
	t.Setenv("PLUGINS_RPC_PER_PLUGIN", "1")
	if _, e := storage.GetDB().Exec(`DELETE FROM app_rate_limits WHERE key=$1`, "rpc:plugin:"+a.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := m.AppRPC(testCtx(), a.ID, token, "identity", json.RawMessage(`{}`)); e != nil {
		t.Fatal(e)
	}
	if _, e := m.AppRPC(testCtx(), a.ID, token, "identity", json.RawMessage(`{}`)); !errors.Is(e, ErrAppRateLimit) {
		t.Fatal("limite RPC plugin", e)
	}
}

// TestAppCleanupWorkerTicks prova início, execução periódica e parada aguardada.
func TestAppCleanupWorkerTicks(t *testing.T) {
	m, u, _, a, channel, _ := interactionFixture(t)
	token := launchSession(t, m, u.ID, a.ID, channel)
	if _, e := storage.GetDB().Exec(`UPDATE app_ui_tokens SET expires_at=now()-interval '1 second' WHERE token_hash=$1`, appHash(token)); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PLUGINS_CLEANUP_SECONDS", "1")
	stop := StartAppInteractionCleanup(context.Background())
	defer stop()
	deadline := time.Now().Add(4 * time.Second)
	for {
		var n int
		if e := storage.GetDB().QueryRow(`SELECT count(*) FROM app_ui_tokens WHERE token_hash=$1`, appHash(token)).Scan(&n); e != nil {
			t.Fatal(e)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker não limpou token expirado")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lifecycleInvite tenta reutilizar contexto de uma fronteira humana no callback de lifecycle.
type lifecycleInvite struct {
	plugins.PluginHost
	output string
}

func (h *lifecycleInvite) Invoke(ctx context.Context, _ string, _ []byte, _ []byte, call plugins.Call) ([]byte, error) {
	out, e := call(ctx, []byte(`{"method":"invite.send","to_user_id":"00000000-0000-0000-0000-000000000001","text":"não deve enviar","data":{}}`))
	h.output = string(out)
	if e != nil {
		return nil, e
	}
	return []byte(`{"ok":true}`), nil
}

// TestAppLifecycleCannotInheritHuman previne convites em on_enable durante hot reload humano.
func TestAppLifecycleCannotInheritHuman(t *testing.T) {
	m, u, _, a, channel, _ := interactionFixture(t)
	b, e := m.bundle(testCtx(), a.AppKey, false)
	if e != nil {
		t.Fatal(e)
	}
	spy := &lifecycleInvite{PluginHost: m.Host}
	m.Host = spy
	ctx := context.WithValue(testCtx(), appHumanKey{}, appHuman{u.ID, channel})
	if _, e = m.invoke(ctx, a, b, "on_enable", nil); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(spy.output, `"ok":false`) {
		t.Fatal("lifecycle herdou humano", spy.output)
	}
}

// TestAppHumanHotReload aplica versão nova na fronteira RPC e exige novo ticket para a UI.
func TestAppHumanHotReload(t *testing.T) {
	m, u, _, a, channel, _ := interactionFixture(t)
	m.Config.HotReload = true
	token := launchSession(t, m, u.ID, a.ID, channel)
	path := filepath.Join(m.Config.Directory, "reference", "ui", "data.json")
	if e := os.WriteFile(path, []byte(`{"message":"versão atualizada"}`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := m.AppRPC(testCtx(), a.ID, token, "identity", json.RawMessage(`{}`)); !errors.Is(e, ErrAppSession) {
		t.Fatal("sessão do digest antigo aceita", e)
	}
	token = launchSession(t, m, u.ID, a.ID, channel)
	if _, e := m.AppRPC(testCtx(), a.ID, token, "identity", json.RawMessage(`{}`)); e != nil {
		t.Fatal("hot reload não aplicado", e)
	}
}

// TestAppTicketConcurrentReplay permite exatamente uma troca concorrente do mesmo segredo.
func TestAppTicketConcurrentReplay(t *testing.T) {
	m, u, _, a, channel, _ := interactionFixture(t)
	ticket, e := m.LaunchAppUI(testCtx(), u.ID, a.ID, LaunchInput{SurfaceID: "home", ChannelID: channel})
	if e != nil {
		t.Fatal(e)
	}
	out := make(chan error, 8)
	for range 8 {
		go func() { _, e := m.ExchangeAppTicket(context.Background(), a.ID, ticket["ticket"].(string)); out <- e }()
	}
	success := 0
	for range 8 {
		if e := <-out; e == nil {
			success++
		} else if !errors.Is(e, ErrAppSession) {
			t.Fatal(e)
		}
	}
	if success != 1 {
		t.Fatal("trocas aceitas", success)
	}
}
