package services

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

// ErrChannelViewNotFound indica view inexistente no canal solicitado.
var ErrChannelViewNotFound = errors.New("view de canal não encontrada")

// validateChannelViewURL aplica as regras de embed, HTTPS, host público e origem distinta.
// BASE_URL vem da configuração, nunca do Host enviado na requisição.
func validateChannelViewURL(ctx context.Context, raw, baseURL string) (string, error) {
	if utf8.RuneCountInString(raw) > 2048 {
		return "", ErrInvalidInput
	}
	_, err := validateEmbedURL(raw)
	if err != nil {
		return "", ErrInvalidInput
	}
	original, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", ErrInvalidInput
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Hostname() == "" {
		return "", ErrInvalidInput
	}
	host := strings.TrimSuffix(strings.ToLower(original.Hostname()), ".")
	if original.Scheme != "https" || strings.EqualFold(host, strings.TrimSuffix(base.Hostname(), ".")) {
		return "", ErrInvalidInput
	}
	if err := utils.ValidatePublicHost(ctx, host); err != nil {
		return "", ErrInvalidInput
	}
	// NormalizeURL remove os colchetes IPv6; preserva a autoridade válida neste contrato.
	u := original
	u.Scheme = "https"
	u.Fragment = ""
	if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	return u.String(), nil
}

// requireChannelViewRead exige UUID válido, canal de texto/voz e leitura pelo ator.
func requireChannelViewRead(ctx context.Context, actor, channel string) error {
	if _, err := uuid.Parse(channel); err != nil {
		return ErrInvalidInput
	}
	ch, err := storage.GetChannelByID(ctx, channel)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrChannelNotFound
	}
	if err != nil {
		return err
	}
	if ch.Type != "text" && ch.Type != "voice" {
		return ErrPermissionDenied
	}
	return requireWebhookChannelRead(ctx, ch, actor)
}

// ListChannelViews entrega views apenas a um leitor humano do canal.
func ListChannelViews(ctx context.Context, actor, channel string) ([]models.ChannelAppView, error) {
	if err := requireChannelViewRead(ctx, actor, channel); err != nil {
		return nil, err
	}
	return storage.ListChannelViews(ctx, channel)
}

// SaveChannelView cria ou substitui URL/posição, sem trocar a instalação de uma view existente.
func SaveChannelView(ctx context.Context, actor, channel, id, app, raw, baseURL string, position int) (models.ChannelAppView, error) {
	var empty models.ChannelAppView
	if err := requireChannelViewRead(ctx, actor, channel); err != nil {
		return empty, err
	}
	if position < 0 || int64(position) > 2147483647 {
		return empty, ErrInvalidInput
	}
	if id != "" {
		if _, err := uuid.Parse(id); err != nil {
			return empty, ErrChannelViewNotFound
		}
		existing, err := storage.GetChannelView(ctx, channel, id)
		if errors.Is(err, storage.ErrNotFound) {
			return empty, ErrChannelViewNotFound
		}
		if err != nil {
			return empty, err
		}
		if app != "" && app != existing.AppInstallationID {
			return empty, ErrInvalidInput
		}
		app = existing.AppInstallationID
	}
	a, err := activeApp(ctx, app)
	if err != nil {
		return empty, err
	}
	if a.Kind != "view" || !a.Capabilities.AttachViews {
		return empty, ErrPermissionDenied
	}
	allowed, err := storage.AppHasScope(ctx, app, channel)
	if err != nil {
		return empty, err
	}
	if !allowed {
		return empty, ErrPermissionDenied
	}
	normalized, err := validateChannelViewURL(ctx, raw, baseURL)
	if err != nil {
		return empty, err
	}
	v, err := storage.SaveChannelView(ctx, actor, app, channel, id, normalized, position)
	if errors.Is(err, storage.ErrNotFound) {
		return empty, ErrPermissionDenied
	}
	if err != nil {
		return empty, err
	}
	action := ActionAppViewCreate
	if id != "" {
		action = ActionAppViewUpdate
	}
	RecordAudit(ctx, AuditEntry{ActorID: actor, Action: action, EntityType: EntityApp, EntityID: &app, Metadata: map[string]any{"channel_id": channel, "view_id": v.ID}})
	return v, nil
}

// DeleteChannelView permite retirar uma view mesmo depois de perder attach_views.
func DeleteChannelView(ctx context.Context, actor, channel, id string) error {
	if err := requireChannelViewRead(ctx, actor, channel); err != nil {
		return err
	}
	if _, err := uuid.Parse(id); err != nil {
		return ErrChannelViewNotFound
	}
	v, err := storage.GetChannelView(ctx, channel, id)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrChannelViewNotFound
	}
	if err != nil {
		return err
	}
	if err = storage.DeleteChannelView(ctx, channel, id); errors.Is(err, storage.ErrNotFound) {
		return ErrChannelViewNotFound
	}
	if err != nil {
		return err
	}
	RecordAudit(ctx, AuditEntry{ActorID: actor, Action: ActionAppViewDelete, EntityType: EntityApp, EntityID: &v.AppInstallationID, Metadata: map[string]any{"channel_id": channel, "view_id": id}})
	return nil
}

// ChannelViewState monta o estado confirmado para broadcast; o chamador deve
// autorizar os destinatários com ChannelReaders antes de distribuir o payload.
func ChannelViewState(ctx context.Context, channelID string) (models.ChannelSummary, error) {
	channel, err := storage.GetChannelSummary(ctx, channelID)
	if err != nil {
		return channel, err
	}
	channel.Views, err = storage.ListChannelViews(ctx, channelID)
	return channel, err
}
