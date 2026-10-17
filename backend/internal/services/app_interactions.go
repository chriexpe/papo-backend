package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// ErrAppSession indica ticket ou sessão UI inválida, expirada ou revogada.
var ErrAppSession = errors.New("sessão do app inválida ou expirada")

const (
	// appTicketTTL é a validade do ticket de lançamento, de uso único.
	appTicketTTL = 60 * time.Second
	// appSessionIdle é a inatividade máxima de uma sessão UI, renovada a cada RPC.
	appSessionIdle = 30 * time.Minute
	// appSessionMax é a duração absoluta máxima de uma sessão UI.
	appSessionMax = 8 * time.Hour
)

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

// InteractionResult é a resposta de POST /interactions: o id da interação e o
// result devolvido pelo on_interaction do plugin, entregue somente a quem
// clicou (null quando o plugin não devolve result).
type InteractionResult struct {
	InteractionID string          `json:"interaction_id"`
	Result        json.RawMessage `json:"result"`
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

// appHuman é a identidade humana verificada de on_interaction e on_ui_call.
type appHuman struct {
	UserID    string
	ChannelID string
}

// uiSession é um ticket ou sessão revalidado, com o convite aceito vinculado e
// a chave do plugin.
type uiSession struct {
	token  models.AppUIToken
	invite *models.AppInvite
	appKey string
}

func appHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// appSecret gera 32 bytes aleatórios em base64url com o prefixo informado.
func appSecret(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HumanCall entrega um callback humano com o perfil do chamador verificado e
// limita a espera ao prazo de interação.
func (m *PluginManager) HumanCall(parent context.Context, key, event string, input json.RawMessage, h appHuman) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.WithValue(parent, appHumanKey{}, h), m.Config.InteractionTimeout)
	defer cancel()
	var payload map[string]json.RawMessage
	if json.Unmarshal(input, &payload) != nil {
		return nil, ErrInvalidInput
	}
	profile, err := callerProfile(ctx, h.UserID)
	if err != nil {
		return nil, err
	}
	payload["user"] = jsonValue(profile)
	if event == "on_interaction" {
		var action InteractionInput
		_ = json.Unmarshal(input, &action)
		if action.Kind == "user_action" {
			target, err := callerProfile(ctx, action.TargetUserID)
			if err != nil {
				return nil, err
			}
			payload["target_user"] = jsonValue(target)
		}
	}
	return m.callEvent(ctx, key, event, jsonValue(payload))
}

// Interact verifica o botão ou a ação persistida, entrega a identidade humana
// ao on_interaction e devolve o result do plugin a quem clicou.
func (m *PluginManager) Interact(ctx context.Context, user string, in InteractionInput) (InteractionResult, error) {
	if uuid.Validate(in.InstallationID) != nil || uuid.Validate(in.ChannelID) != nil {
		return InteractionResult{}, ErrInvalidInput
	}
	if err := CanReadChannel(ctx, in.ChannelID, user); err != nil {
		return InteractionResult{}, err
	}
	b, err := ResolvePluginSurfaceBundle(ctx, in.InstallationID)
	if err != nil {
		return InteractionResult{}, err
	}
	switch in.Kind {
	case "button":
		err = validateButtonInteraction(ctx, in)
	case "user_action":
		err = validateUserAction(ctx, user, in, b)
	default:
		err = ErrInvalidInput
	}
	if err != nil {
		return InteractionResult{}, err
	}
	if err = m.limit("interaction:user:"+user, m.Config.Limits.InteractionsPerUser); err != nil {
		return InteractionResult{}, err
	}
	if err = m.limit("interaction:plugin:"+in.InstallationID, m.Config.Limits.InteractionsPerPlugin); err != nil {
		return InteractionResult{}, err
	}
	id := uuid.NewString()
	payload := map[string]any{"interaction_id": id, "kind": in.Kind, "user_id": user, "channel_id": in.ChannelID}
	optional := map[string]string{
		"message_id":     in.MessageID,
		"component_id":   in.ComponentID,
		"action_id":      in.ActionID,
		"target_user_id": in.TargetUserID,
	}
	for k, v := range optional {
		if v != "" {
			payload[k] = v
		}
	}
	out, err := m.HumanCall(ctx, b.AppKey, "on_interaction", jsonValue(payload), appHuman{UserID: user, ChannelID: in.ChannelID})
	if err != nil {
		return InteractionResult{}, err
	}
	return InteractionResult{InteractionID: id, Result: pluginResult(out)}, nil
}

// pluginResult extrai o result do envelope {"ok":true,"result":...} do guest.
func pluginResult(out []byte) json.RawMessage {
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(out, &envelope) != nil || len(envelope.Result) == 0 {
		return json.RawMessage("null")
	}
	return envelope.Result
}

// validateButtonInteraction exige um botão ativo, sem opens, de mensagem do
// próprio plugin no canal informado.
func validateButtonInteraction(ctx context.Context, in InteractionInput) error {
	if in.MessageID == "" || in.ComponentID == "" || in.ActionID != "" || in.TargetUserID != "" {
		return ErrInvalidInput
	}
	msg, err := storage.GetMessageByID(ctx, in.MessageID)
	if err != nil {
		return ErrInvalidInput
	}
	if msg.ChannelID != in.ChannelID || msg.AppInstallationID == nil || *msg.AppInstallationID != in.InstallationID {
		return ErrPermissionDenied
	}
	all, err := storage.AppComponents(ctx, []string{msg.ID})
	if err != nil {
		return err
	}
	for _, row := range all[msg.ID] {
		for _, c := range row {
			if c.ID == in.ComponentID && !c.Disabled && c.Opens == "" {
				return nil
			}
		}
	}
	return ErrInvalidInput
}

// validateUserAction exige uma ação declarada, no canal app do próprio plugin,
// apontando para outro leitor do canal.
func validateUserAction(ctx context.Context, user string, in InteractionInput, b PluginSurfaceBundle) error {
	ch, err := storage.GetAppChannel(ctx, in.ChannelID)
	if err != nil || ch.AppInstallationID != in.InstallationID {
		return ErrPermissionDenied
	}
	if in.MessageID != "" || in.ComponentID != "" || in.TargetUserID == user || uuid.Validate(in.TargetUserID) != nil {
		return ErrInvalidInput
	}
	if err = CanReadChannel(ctx, in.ChannelID, in.TargetUserID); err != nil {
		return err
	}
	for _, a := range b.UserActions {
		if a.ID == in.ActionID {
			return nil
		}
	}
	return ErrInvalidInput
}

// ValidateAppButtons impede referências a superfícies de outra instalação.
func ValidateAppButtons(ctx context.Context, id string, rows [][]models.AppButton) error {
	if len(rows) > 2 {
		return ErrInvalidInput
	}
	var b PluginSurfaceBundle
	if len(rows) > 0 {
		var err error
		if b, err = ResolvePluginSurfaceBundle(ctx, id); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if len(row) == 0 || len(row) > 5 {
			return ErrInvalidInput
		}
		for _, v := range row {
			if !validButton(v) || seen[v.ID] {
				return ErrInvalidInput
			}
			seen[v.ID] = true
			if v.Opens != "" {
				if _, err := embedSurface(b, v.Opens); err != nil {
					return ErrInvalidInput
				}
			}
		}
	}
	return nil
}

// validButton limita id, rótulo e estilo de um botão.
func validButton(v models.AppButton) bool {
	if v.ID == "" || len(v.ID) > 64 || !utf8.ValidString(v.ID) {
		return false
	}
	if strings.TrimSpace(v.Label) == "" || !utf8.ValidString(v.Label) || utf8.RuneCountInString(v.Label) > 80 {
		return false
	}
	return v.Style == "primary" || v.Style == "secondary" || v.Style == "danger"
}

// LaunchAppUI gera ticket de uso único após verificar a referência recebida pelo cliente.
func (m *PluginManager) LaunchAppUI(ctx context.Context, user, id string, in LaunchInput) (map[string]any, error) {
	if err := CanReadChannel(ctx, in.ChannelID, user); err != nil {
		return nil, err
	}
	b, err := ResolvePluginSurfaceBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	selected, err := launchSurface(ctx, id, b, in)
	if err != nil {
		return nil, err
	}
	if in.InviteID != "" {
		v, err := storage.GetAppInvite(ctx, in.InviteID, user)
		if err != nil || v.Status != "accepted" || v.InstallationID != id || v.ChannelID != in.ChannelID {
			return nil, ErrPermissionDenied
		}
	}
	if err = m.limit("launch:"+user, m.Config.Limits.LaunchPerUser); err != nil {
		return nil, err
	}
	secret, err := appSecret("pat_")
	if err != nil {
		return nil, err
	}
	generation, err := storage.AppUIGeneration(ctx, id)
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(appTicketTTL)
	t := models.AppUIToken{
		ID:                uuid.NewString(),
		Hash:              appHash(secret),
		Kind:              "ticket",
		UserID:            user,
		InstallationID:    id,
		ChannelID:         in.ChannelID,
		SurfaceID:         in.SurfaceID,
		Digest:            b.Digest,
		Generation:        generation,
		ExpiresAt:         expires,
		AbsoluteExpiresAt: expires,
	}
	if in.MessageID != "" {
		t.MessageID = &in.MessageID
	}
	if in.InviteID != "" {
		t.InviteID = &in.InviteID
	}
	if err = storage.SaveAppUIToken(ctx, t); err != nil {
		return nil, err
	}
	entry := selected.Entry
	if selected.Source == "bundled" {
		entry = pluginAssetURL(id, b.Digest, entry)
	}
	poster := selected.Poster
	if poster != "" {
		poster = pluginAssetURL(id, b.Digest, poster)
	}
	surface := models.AppChannel{
		AppInstallationID: id,
		SurfaceID:         selected.ID,
		Source:            selected.Source,
		Entry:             entry,
		AvatarURL:         "/apps/" + id + "/avatar",
		Available:         true,
	}
	return map[string]any{
		"ticket":     secret,
		"expires_at": t.ExpiresAt,
		"surface":    surface,
		"display":    selected.Display,
		"load":       selected.Load,
		"size":       selected.Size,
		"poster":     poster,
	}, nil
}

// launchSurface exige que a superfície pedida seja o canal app informado ou
// esteja referenciada (frame ou botão opens ativo) na mensagem informada.
func launchSurface(ctx context.Context, id string, b PluginSurfaceBundle, in LaunchInput) (AppSurfaceDeclaration, error) {
	for _, s := range b.Surfaces {
		if s.ID != in.SurfaceID {
			continue
		}
		if s.Kind == "channel" {
			ch, err := storage.GetAppChannel(ctx, in.ChannelID)
			if err == nil && ch.AppInstallationID == id && ch.SurfaceID == s.ID {
				return s, nil
			}
			return AppSurfaceDeclaration{}, ErrPermissionDenied
		}
		if in.MessageID == "" {
			return AppSurfaceDeclaration{}, ErrPermissionDenied
		}
		msg, err := storage.GetMessageByID(ctx, in.MessageID)
		if err != nil || msg.ChannelID != in.ChannelID || msg.AppInstallationID == nil || *msg.AppInstallationID != id {
			return AppSurfaceDeclaration{}, ErrPermissionDenied
		}
		embeds, err := ListMessageEmbeds(ctx, []string{msg.ID})
		if err != nil {
			return AppSurfaceDeclaration{}, err
		}
		for _, v := range embeds[msg.ID] {
			if v.Surface != nil && v.Surface.ID == s.ID {
				return s, nil
			}
		}
		buttons, err := storage.AppComponents(ctx, []string{msg.ID})
		if err != nil {
			return AppSurfaceDeclaration{}, err
		}
		for _, row := range buttons[msg.ID] {
			for _, v := range row {
				if v.Opens == s.ID && !v.Disabled {
					return s, nil
				}
			}
		}
		return AppSurfaceDeclaration{}, ErrPermissionDenied
	}
	return AppSurfaceDeclaration{}, ErrPermissionDenied
}

// validateUIToken refaz a autorização em toda troca e chamada: plugin
// habilitado no mesmo digest, leitura do canal e convite ainda aceito. Perda
// de leitura ou digest diferente apagam os tokens do usuário na instalação.
func (m *PluginManager) validateUIToken(ctx context.Context, id, secret, kind string) (uiSession, error) {
	t, err := storage.GetAppUIToken(ctx, appHash(secret), kind, id)
	if err != nil {
		return uiSession{}, ErrAppSession
	}
	b, err := ResolvePluginSurfaceBundle(ctx, id)
	if transientPluginError(err) {
		return uiSession{}, err
	}
	if err != nil || b.Digest != t.Digest {
		_ = storage.RevokeUserAppUI(ctx, t.UserID, id)
		return uiSession{}, ErrAppSession
	}
	if err = CanReadChannel(ctx, t.ChannelID, t.UserID); err != nil {
		_ = storage.RevokeUserAppUI(ctx, t.UserID, id)
		return uiSession{}, ErrAppSession
	}
	s := uiSession{token: t, appKey: b.AppKey}
	if t.InviteID != nil {
		v, err := storage.GetAppInvite(ctx, *t.InviteID, t.UserID)
		if err != nil || v.Status != "accepted" {
			return uiSession{}, ErrAppSession
		}
		s.invite = &v
	}
	return s, nil
}

// ExchangeAppTicket consome atomicamente o ticket e guarda somente o hash da sessão.
func (m *PluginManager) ExchangeAppTicket(ctx context.Context, id, secret string) (map[string]any, error) {
	s, err := m.validateUIToken(ctx, id, secret, "ticket")
	if err != nil {
		return nil, err
	}
	t := s.token
	ok, err := storage.ConsumeAppTicket(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrAppSession
	}
	secret, err = appSecret("pas_")
	if err != nil {
		return nil, err
	}
	t.ID = uuid.NewString()
	t.Hash = appHash(secret)
	t.Kind = "session"
	t.ExpiresAt = time.Now().Add(appSessionIdle)
	t.AbsoluteExpiresAt = time.Now().Add(appSessionMax)
	if err = storage.SaveAppUIToken(ctx, t); err != nil {
		return nil, err
	}
	return map[string]any{
		"token":               secret,
		"session_id":          t.ID,
		"expires_at":          t.ExpiresAt,
		"absolute_expires_at": t.AbsoluteExpiresAt,
	}, nil
}

// AppRPC entrega on_ui_call com a identidade verificada da sessão e só renova
// a inatividade depois de uma chamada válida. A renovação exige, no próprio
// UPDATE, a mesma geração e o plugin habilitado, então uma sessão revogada
// durante a chamada não é estendida.
func (m *PluginManager) AppRPC(ctx context.Context, id, secret, method string, params json.RawMessage) ([]byte, error) {
	if method == "" || len(method) > 100 || len(params) > 32768 || !json.Valid(params) {
		return nil, ErrInvalidInput
	}
	s, err := m.validateUIToken(ctx, id, secret, "session")
	if err != nil {
		return nil, err
	}
	t := s.token
	if err = m.limit("rpc:session:"+t.ID, m.Config.Limits.RPCPerSession); err != nil {
		return nil, err
	}
	if err = m.limit("rpc:plugin:"+id, m.Config.Limits.RPCPerPlugin); err != nil {
		return nil, err
	}
	input := map[string]any{
		"user_id":    t.UserID,
		"channel_id": t.ChannelID,
		"session_id": t.ID,
		"method":     method,
		"params":     params,
	}
	if s.invite != nil {
		input["invite"] = *s.invite
	}
	out, err := m.HumanCall(ctx, s.appKey, "on_ui_call", jsonValue(input), appHuman{UserID: t.UserID, ChannelID: t.ChannelID})
	if err != nil {
		return nil, err
	}
	if err = storage.TouchAppSession(ctx, t.ID, appSessionIdle); err != nil {
		return nil, ErrAppSession
	}
	return out, nil
}

// SendAppInvite aceita apenas callbacks com identidade humana verificada.
func (m *PluginManager) SendAppInvite(ctx context.Context, a models.AppInstallation, target, text string, data json.RawMessage) (models.AppInvite, error) {
	h, ok := ctx.Value(appHumanKey{}).(appHuman)
	if !ok || !a.Capabilities.SendInvites {
		return models.AppInvite{}, ErrPermissionDenied
	}
	ch, err := storage.GetAppChannel(ctx, h.ChannelID)
	if err != nil || ch.AppInstallationID != a.ID {
		return models.AppInvite{}, ErrPermissionDenied
	}
	if target == h.UserID || uuid.Validate(target) != nil {
		return models.AppInvite{}, ErrInvalidInput
	}
	if strings.TrimSpace(text) == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > 500 {
		return models.AppInvite{}, ErrInvalidInput
	}
	if len(data) > 8192 || !json.Valid(data) {
		return models.AppInvite{}, ErrInvalidInput
	}
	if err = CanReadChannel(ctx, h.ChannelID, h.UserID); err != nil {
		return models.AppInvite{}, err
	}
	if err = CanReadChannel(ctx, h.ChannelID, target); err != nil {
		return models.AppInvite{}, err
	}
	if err = m.limit("invite:sender:"+h.UserID, m.Config.Limits.InvitesPerSender); err != nil {
		return models.AppInvite{}, err
	}
	if err = m.limit("invite:target:"+target, m.Config.Limits.InvitesPerTarget); err != nil {
		return models.AppInvite{}, err
	}
	v := models.AppInvite{
		SurfaceID:      ch.SurfaceID,
		ID:             uuid.NewString(),
		UserID:         target,
		InstallationID: a.ID,
		InviterID:      h.UserID,
		ChannelID:      h.ChannelID,
		Text:           text,
		Data:           data,
		Status:         "pending",
		ExpiresAt:      time.Now().Add(m.Config.InviteTTL),
	}
	push := jsonValue(map[string]any{
		"type":                "app_invite",
		"notification_id":     v.ID,
		"app_installation_id": a.ID,
		"channel_id":          h.ChannelID,
		"author":              a.Name,
		"preview":             text,
	})
	if err = storage.CreateAppInvite(ctx, v, string(push)); err != nil {
		return models.AppInvite{}, err
	}
	RecordAudit(ctx, AuditEntry{
		ActorID:      h.UserID,
		Action:       ActionAppInviteSend,
		EntityType:   EntityApp,
		EntityID:     &a.ID,
		TargetUserID: &target,
	})
	if m.Effects.Invite != nil {
		m.Effects.Invite(ctx, v)
	}
	return v, nil
}

// DecideInvite valida acesso antes da decisão e não entrega ticket automaticamente.
func (m *PluginManager) DecideInvite(ctx context.Context, user, id, status string) (models.AppInvite, error) {
	v, err := storage.GetAppInvite(ctx, id, user)
	if err != nil {
		return v, ErrNotificationNotFound
	}
	if status == "accepted" {
		if err = CanReadChannel(ctx, v.ChannelID, user); err != nil {
			return v, err
		}
		if _, err = ResolvePluginSurfaceBundle(ctx, v.InstallationID); err != nil {
			return v, err
		}
	}
	if err = storage.DecideAppInvite(ctx, id, user, status); err != nil {
		return v, ErrInvalidInput
	}
	v.Status = status
	RecordAudit(ctx, AuditEntry{
		ActorID:    user,
		Action:     ActionAppInviteDecide,
		EntityType: EntityApp,
		EntityID:   &v.InstallationID,
		Metadata:   map[string]any{"decision": status},
	})
	return v, nil
}

// StartInteractionCleanup remove periodicamente tickets, sessões e convites
// expirados e descarta limites em memória sem uso. A função retornada cancela o
// worker e aguarda o fim do ciclo em andamento.
func (m *PluginManager) StartInteractionCleanup(parent context.Context) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(m.Config.CleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.limits.sweep(time.Now())
				c, stop := context.WithTimeout(ctx, 10*time.Second)
				if err := storage.CleanAppInteractions(c); err != nil && !errors.Is(err, context.Canceled) {
					utils.Errorf("limpeza de interações: %v", err)
				}
				stop()
			}
		}
	}()
	return func() { cancel(); <-done }
}

// SetPluginInviteMute altera a preferência do próprio usuário para uma instalação existente.
func SetPluginInviteMute(ctx context.Context, user, id string, muted bool) error {
	a, err := storage.GetApp(ctx, id)
	if err != nil {
		return ErrAppNotFound
	}
	if a.Kind != "plugin" {
		return ErrInvalidInput
	}
	return storage.SetAppInviteMute(ctx, user, id, muted)
}
