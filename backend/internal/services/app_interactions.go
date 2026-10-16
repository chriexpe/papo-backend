package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

// ErrAppRateLimit preserva uma resposta curta e não expõe estado de outros usuários.
var ErrAppRateLimit = errors.New("limite de chamadas do app excedido")
var ErrAppSession = errors.New("sessão do app inválida ou expirada")

// InteractionInput contém apenas seletores; identidade é preenchida pelo servidor.
type InteractionInput struct {
	InstallationID string `json:"app_installation_id"`
	Kind           string `json:"kind"`
	ChannelID      string `json:"channel_id"`
	MessageID      string `json:"message_id,omitempty"`
	ComponentID    string `json:"component_id,omitempty"`
	ActionID       string `json:"action_id,omitempty"`
	TargetUserID   string `json:"target_user_id,omitempty"`
}

// LaunchInput vincula uma superfície a um contexto legível, nunca a uma URL arbitrária.
type LaunchInput struct {
	SurfaceID string `json:"surface_id"`
	ChannelID string `json:"channel_id"`
	MessageID string `json:"message_id,omitempty"`
	InviteID  string `json:"invite_id,omitempty"`
}
type appHumanKey struct{}
type appButtonsKey struct{}
type appHuman struct{ UserID, ChannelID string }

// appInt lê limites positivos, mantendo defaults seguros para valores inválidos.
func appInt(key string, fallback int) int {
	n, e := strconv.Atoi(os.Getenv(key))
	if e != nil || n < 1 {
		return fallback
	}
	return n
}
func appLimit(ctx context.Context, key, env string, n int) error {
	ok, e := storage.AppRateLimit(ctx, key, appInt(env, n))
	if e != nil {
		return e
	}
	if !ok {
		return ErrAppRateLimit
	}
	return nil
}
func appHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func appSecret(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HumanCall aplica a mesma fronteira de upgrade do trigger e limita a espera na fila.
func (m *PluginManager) HumanCall(parent context.Context, id, event string, input json.RawMessage, h appHuman) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.WithValue(parent, appHumanKey{}, h), time.Duration(appInt("PLUGINS_INTERACTION_TIMEOUT_MS", 3000))*time.Millisecond)
	defer cancel()
	return m.callEvent(ctx, id, event, input)
}

// Interact verifica o botão/ação persistido antes de entregar a identidade humana.
func (m *PluginManager) Interact(ctx context.Context, user string, in InteractionInput) (string, error) {
	if uuid.Validate(in.InstallationID) != nil || uuid.Validate(in.ChannelID) != nil {
		return "", ErrInvalidInput
	}
	if e := CanReadChannel(ctx, in.ChannelID, user); e != nil {
		return "", e
	}
	if e := m.refreshHumanPlugin(ctx, in.InstallationID); e != nil {
		return "", e
	}
	b, e := ResolvePluginSurfaceBundle(ctx, in.InstallationID)
	if e != nil {
		return "", e
	}
	switch in.Kind {
	case "button":
		if in.MessageID == "" || in.ComponentID == "" || in.ActionID != "" || in.TargetUserID != "" {
			return "", ErrInvalidInput
		}
		msg, e := storage.GetMessageByID(ctx, in.MessageID)
		if e != nil {
			return "", ErrInvalidInput
		}
		if msg.ChannelID != in.ChannelID || msg.AppInstallationID == nil || *msg.AppInstallationID != in.InstallationID {
			return "", ErrPermissionDenied
		}
		all, e := storage.AppComponents(ctx, []string{msg.ID})
		if e != nil {
			return "", e
		}
		found := false
		for _, row := range all[msg.ID] {
			for _, c := range row {
				if c.ID == in.ComponentID && !c.Disabled && c.Opens == "" {
					found = true
				}
			}
		}
		if !found {
			return "", ErrInvalidInput
		}
	case "user_action":
		ch, e := storage.GetAppChannel(ctx, in.ChannelID)
		if e != nil || ch.AppInstallationID != in.InstallationID {
			return "", ErrPermissionDenied
		}
		if in.MessageID != "" || in.ComponentID != "" || in.TargetUserID == user || uuid.Validate(in.TargetUserID) != nil {
			return "", ErrInvalidInput
		}
		if e = CanReadChannel(ctx, in.ChannelID, in.TargetUserID); e != nil {
			return "", e
		}
		found := false
		for _, a := range b.UserActions {
			if a.ID == in.ActionID {
				found = true
			}
		}
		if !found {
			return "", ErrInvalidInput
		}
	default:
		return "", ErrInvalidInput
	}
	if e = appLimit(ctx, "interaction:user:"+user, "PLUGINS_INTERACTIONS_PER_USER", 30); e != nil {
		return "", e
	}
	if e = appLimit(ctx, "interaction:plugin:"+in.InstallationID, "PLUGINS_INTERACTIONS_PER_PLUGIN", 120); e != nil {
		return "", e
	}
	id := uuid.NewString()
	payload := map[string]any{"interaction_id": id, "kind": in.Kind, "user_id": user, "channel_id": in.ChannelID}
	for k, v := range map[string]string{"message_id": in.MessageID, "component_id": in.ComponentID, "action_id": in.ActionID, "target_user_id": in.TargetUserID} {
		if v != "" {
			payload[k] = v
		}
	}
	a, e := storage.GetApp(ctx, in.InstallationID)
	if e != nil {
		return "", e
	}
	_, e = m.HumanCall(ctx, a.AppKey, "on_interaction", jsonValue(payload), appHuman{user, in.ChannelID})
	if e == nil {
		RecordAudit(ctx, AuditEntry{ActorID: user, Action: ActionAppInteraction, EntityType: EntityApp, EntityID: &in.InstallationID})
	}
	return id, e
}

// ValidateAppButtons impede referências a superfícies de outra instalação.
func ValidateAppButtons(ctx context.Context, id string, rows [][]models.AppButton) error {
	if len(rows) > 2 {
		return ErrInvalidInput
	}
	seen := map[string]bool{}
	var b PluginSurfaceBundle
	var e error
	if len(rows) > 0 {
		b, e = ResolvePluginSurfaceBundle(ctx, id)
		if e != nil {
			return e
		}
	}
	for _, row := range rows {
		if len(row) == 0 || len(row) > 5 {
			return ErrInvalidInput
		}
		for _, v := range row {
			if v.ID == "" || len(v.ID) > 64 || seen[v.ID] || !utf8.ValidString(v.ID) || strings.TrimSpace(v.Label) == "" || !utf8.ValidString(v.Label) || utf8.RuneCountInString(v.Label) > 80 {
				return ErrInvalidInput
			}
			seen[v.ID] = true
			if v.Style != "primary" && v.Style != "secondary" && v.Style != "danger" {
				return ErrInvalidInput
			}
			if v.Opens != "" {
				found := false
				for _, s := range b.Surfaces {
					if s.Kind == "embed" && s.ID == v.Opens {
						found = true
					}
				}
				if !found {
					return ErrInvalidInput
				}
			}
		}
	}
	return nil
}

// LaunchAppUI gera ticket de uso único após verificar a referência recebida pelo cliente.
func (m *PluginManager) LaunchAppUI(ctx context.Context, user, id string, in LaunchInput) (map[string]any, error) {
	if e := CanReadChannel(ctx, in.ChannelID, user); e != nil {
		return nil, e
	}
	b, e := ResolvePluginSurfaceBundle(ctx, id)
	if e != nil {
		return nil, e
	}
	found := false
	for _, s := range b.Surfaces {
		if s.ID != in.SurfaceID {
			continue
		}
		if s.Kind == "channel" {
			ch, e := storage.GetAppChannel(ctx, in.ChannelID)
			found = e == nil && ch.AppInstallationID == id && ch.SurfaceID == s.ID
		} else if in.MessageID != "" {
			msg, e := storage.GetMessageByID(ctx, in.MessageID)
			if e != nil || msg.ChannelID != in.ChannelID || msg.AppInstallationID == nil || *msg.AppInstallationID != id {
				return nil, ErrPermissionDenied
			}
			embeds, e := ListMessageEmbeds(ctx, []string{msg.ID})
			if e != nil {
				return nil, e
			}
			for _, v := range embeds[msg.ID] {
				if v.Surface != nil && v.Surface.ID == s.ID {
					found = true
				}
			}
			buttons, e := storage.AppComponents(ctx, []string{msg.ID})
			if e != nil {
				return nil, e
			}
			for _, row := range buttons[msg.ID] {
				for _, v := range row {
					if v.Opens == s.ID && !v.Disabled {
						found = true
					}
				}
			}
		}
	}
	if !found {
		return nil, ErrPermissionDenied
	}
	if in.InviteID != "" {
		v, e := storage.GetAppInvite(ctx, in.InviteID, user)
		if e != nil || v.Status != "accepted" || v.InstallationID != id || v.ChannelID != in.ChannelID {
			return nil, ErrPermissionDenied
		}
	}
	if e = appLimit(ctx, "launch:"+user, "PLUGINS_LAUNCH_PER_USER", 60); e != nil {
		return nil, e
	}
	secret, e := appSecret("pat_")
	if e != nil {
		return nil, e
	}
	generation, e := storage.AppUIGeneration(ctx, id)
	if e != nil {
		return nil, e
	}
	t := models.AppUIToken{ID: uuid.NewString(), Hash: appHash(secret), Kind: "ticket", UserID: user, InstallationID: id, ChannelID: in.ChannelID, SurfaceID: in.SurfaceID, Digest: b.Digest, Generation: generation, ExpiresAt: time.Now().Add(60 * time.Second), AbsoluteExpiresAt: time.Now().Add(60 * time.Second)}
	if in.MessageID != "" {
		t.MessageID = &in.MessageID
	}
	if in.InviteID != "" {
		t.InviteID = &in.InviteID
	}
	if e = storage.SaveAppUIToken(ctx, t); e != nil {
		return nil, e
	}
	RecordAudit(ctx, AuditEntry{ActorID: user, Action: ActionAppUILaunch, EntityType: EntityApp, EntityID: &id})
	return map[string]any{"ticket": secret, "expires_at": t.ExpiresAt}, nil
}

// validateUIToken refaz autorização em toda troca e chamada; falhas revogam os vínculos.
func (m *PluginManager) validateUIToken(ctx context.Context, id, secret, kind string) (models.AppUIToken, error) {
	t, e := storage.GetAppUIToken(ctx, appHash(secret), kind, id)
	if e != nil {
		return t, ErrAppSession
	}
	if e = m.refreshHumanPlugin(ctx, id); e != nil {
		if transientPluginError(e) {
			return t, e
		}
		return t, ErrAppSession
	}
	b, e := ResolvePluginSurfaceBundle(ctx, id)
	if transientPluginError(e) {
		return t, e
	}
	if e != nil || b.Digest != t.Digest {
		_ = storage.RevokeUserAppUI(ctx, t.UserID, id)
		return t, ErrAppSession
	}
	if e = CanReadChannel(ctx, t.ChannelID, t.UserID); e != nil {
		_ = storage.RevokeUserAppUI(ctx, t.UserID, id)
		return t, ErrAppSession
	}
	if t.InviteID != nil {
		v, e := storage.GetAppInvite(ctx, *t.InviteID, t.UserID)
		if e != nil || v.Status != "accepted" {
			return t, ErrAppSession
		}
	}
	return t, nil
}

// ExchangeAppTicket consome atomicamente o ticket e guarda somente o hash da sessão.
func (m *PluginManager) ExchangeAppTicket(ctx context.Context, id, secret string) (map[string]any, error) {
	t, e := m.validateUIToken(ctx, id, secret, "ticket")
	if e != nil {
		return nil, e
	}
	ok, e := storage.ConsumeAppTicket(ctx, t.ID)
	if e != nil {
		return nil, e
	}
	if !ok {
		return nil, ErrAppSession
	}
	secret, e = appSecret("pas_")
	if e != nil {
		return nil, e
	}
	t.ID = uuid.NewString()
	t.Hash = appHash(secret)
	t.Kind = "session"
	t.ExpiresAt = time.Now().Add(30 * time.Minute)
	t.AbsoluteExpiresAt = time.Now().Add(8 * time.Hour)
	if e = storage.SaveAppUIToken(ctx, t); e != nil {
		return nil, e
	}
	RecordAudit(ctx, AuditEntry{ActorID: t.UserID, Action: ActionAppUISession, EntityType: EntityApp, EntityID: &id})
	return map[string]any{"token": secret, "session_id": t.ID, "expires_at": t.ExpiresAt, "absolute_expires_at": t.AbsoluteExpiresAt}, nil
}

// AppRPC usa identidade verificada e estende somente uma chamada válida.
func (m *PluginManager) AppRPC(ctx context.Context, id, secret, method string, params json.RawMessage) ([]byte, error) {
	if method == "" || len(method) > 100 || len(params) > 32768 || !json.Valid(params) {
		return nil, ErrInvalidInput
	}
	t, e := m.validateUIToken(ctx, id, secret, "session")
	if e != nil {
		return nil, e
	}
	if e = appLimit(ctx, "rpc:session:"+t.ID, "PLUGINS_RPC_PER_SESSION", 120); e != nil {
		return nil, e
	}
	if e = appLimit(ctx, "rpc:plugin:"+id, "PLUGINS_RPC_PER_PLUGIN", 600); e != nil {
		return nil, e
	}
	input := map[string]any{"user_id": t.UserID, "channel_id": t.ChannelID, "session_id": t.ID, "method": method, "params": params}
	if t.InviteID != nil {
		v, e := storage.GetAppInvite(ctx, *t.InviteID, t.UserID)
		if e != nil {
			return nil, ErrAppSession
		}
		input["invite"] = v
	}
	a, e := storage.GetApp(ctx, id)
	if e != nil {
		return nil, e
	}
	out, e := m.HumanCall(ctx, a.AppKey, "on_ui_call", jsonValue(input), appHuman{t.UserID, t.ChannelID})
	if e != nil {
		return nil, e
	}
	if _, e = m.validateUIToken(ctx, id, secret, "session"); e != nil {
		return nil, e
	}
	if e = storage.TouchAppSession(ctx, t.ID, 30*time.Minute); e != nil {
		return nil, ErrAppSession
	}
	RecordAudit(ctx, AuditEntry{ActorID: t.UserID, Action: ActionAppUICall, EntityType: EntityApp, EntityID: &id, Metadata: map[string]any{"method": method}})
	return out, nil
}

// SendAppInvite aceita apenas callbacks com identidade humana verificada.
func (m *PluginManager) SendAppInvite(ctx context.Context, a models.AppInstallation, target, text string, data json.RawMessage) (models.AppInvite, error) {
	var empty models.AppInvite
	h, ok := ctx.Value(appHumanKey{}).(appHuman)
	if !ok || !a.Capabilities.SendInvites {
		return empty, ErrPermissionDenied
	}
	ch, e := storage.GetAppChannel(ctx, h.ChannelID)
	if e != nil || ch.AppInstallationID != a.ID {
		return empty, ErrPermissionDenied
	}
	if target == h.UserID || uuid.Validate(target) != nil || strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > 500 || !utf8.ValidString(text) || len(data) > 8192 || !json.Valid(data) {
		return empty, ErrInvalidInput
	}
	if e = CanReadChannel(ctx, h.ChannelID, h.UserID); e != nil {
		return empty, e
	}
	if e = CanReadChannel(ctx, h.ChannelID, target); e != nil {
		return empty, e
	}
	if e = appLimit(ctx, "invite:sender:"+h.UserID, "PLUGINS_INVITES_PER_SENDER", 10); e != nil {
		return empty, e
	}
	if e = appLimit(ctx, "invite:target:"+target, "PLUGINS_INVITES_PER_TARGET", 5); e != nil {
		return empty, e
	}
	v := models.AppInvite{SurfaceID: ch.SurfaceID, ID: uuid.NewString(), UserID: target, InstallationID: a.ID, InviterID: h.UserID, ChannelID: h.ChannelID, Text: text, Data: data, Status: "pending", ExpiresAt: time.Now().Add(time.Duration(appInt("PLUGINS_INVITE_TTL_SECONDS", 86400)) * time.Second)}
	push := jsonValue(map[string]any{"type": "app_invite", "notification_id": v.ID, "app_installation_id": a.ID, "channel_id": h.ChannelID, "author": a.Name, "preview": text})
	if e = storage.CreateAppInvite(ctx, v, string(push)); e != nil {
		return empty, e
	}
	RecordAudit(ctx, AuditEntry{ActorID: h.UserID, Action: ActionAppInviteSend, EntityType: EntityApp, EntityID: &a.ID, TargetUserID: &target})
	if m.Effects.Invite != nil {
		m.Effects.Invite(ctx, v)
	}
	return v, nil
}

// DecideInvite valida acesso antes da decisão e não entrega ticket automaticamente.
func (m *PluginManager) DecideInvite(ctx context.Context, user, id, status string) (models.AppInvite, error) {
	v, e := storage.GetAppInvite(ctx, id, user)
	if e != nil {
		return v, ErrNotificationNotFound
	}
	if status == "accepted" {
		if e = CanReadChannel(ctx, v.ChannelID, user); e != nil {
			return v, e
		}
		if _, e = ResolvePluginSurfaceBundle(ctx, v.InstallationID); e != nil {
			return v, e
		}
	}
	if e = storage.DecideAppInvite(ctx, id, user, status); e != nil {
		return v, ErrInvalidInput
	}
	v.Status = status
	RecordAudit(ctx, AuditEntry{ActorID: user, Action: ActionAppInviteDecide, EntityType: EntityApp, EntityID: &v.InstallationID, Metadata: map[string]any{"decision": status}})
	return v, nil
}

// StartAppInteractionCleanup acompanha o ciclo do servidor e aguarda parada cooperativa.
func StartAppInteractionCleanup(parent context.Context) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Duration(appInt("PLUGINS_CLEANUP_SECONDS", 60)) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c, stop := context.WithTimeout(ctx, 10*time.Second)
				if e := storage.CleanAppInteractions(c); e != nil && !errors.Is(e, context.Canceled) {
					utils.Errorf("limpeza de interações: %v", e)
				}
				stop()
			}
		}
	}()
	return func() { cancel(); <-done }
}

// SetPluginInviteMute altera a preferência do próprio usuário para uma instalação existente.
func SetPluginInviteMute(ctx context.Context, user, id string, muted bool) error {
	a, e := storage.GetApp(ctx, id)
	if e != nil {
		return ErrAppNotFound
	}
	if a.Kind != "plugin" {
		return ErrInvalidInput
	}
	if e = storage.SetAppInviteMute(ctx, user, id, muted); e != nil {
		return e
	}
	RecordAudit(ctx, AuditEntry{ActorID: user, Action: ActionAppInviteMute, EntityType: EntityApp, EntityID: &id})
	return nil
}

// refreshHumanPlugin aplica hot reload na fronteira humana; novas versões revogam as sessões antigas.
func (m *PluginManager) refreshHumanPlugin(ctx context.Context, id string) error {
	a, e := activeApp(ctx, id)
	if e != nil {
		return e
	}
	if a.Kind != "plugin" || a.PluginState != "enabled" || a.LifecyclePending {
		return ErrPermissionDenied
	}
	b, e := m.bundle(ctx, a.AppKey, false)
	if transientPluginError(e) {
		return e
	}
	if e == nil && a.PluginDigest != nil && b.Digest == *a.PluginDigest {
		return nil
	}
	unlock, e := m.lock(ctx, a.AppKey)
	if e != nil {
		return e
	}
	defer unlock()
	a, e = activeApp(ctx, id)
	if e != nil {
		return e
	}
	b, e = m.bundle(ctx, a.AppKey, false)
	if transientPluginError(e) {
		return e
	}
	if e == nil && a.PluginDigest != nil && b.Digest == *a.PluginDigest {
		return nil
	}
	if !m.Config.HotReload || e != nil {
		_ = m.quarantine(ctx, a)
		return ErrPluginConsent
	}
	return m.reloadLocked(ctx, a)
}
