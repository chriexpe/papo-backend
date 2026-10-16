package services

import (
	"context"
	"papo/internal/models"
)

// messageSender identifica quem envia: exatamente um campo preenchido.
type messageSender struct {
	authorID          string
	webhookID         string
	appInstallationID string
}

// valid exige exatamente um principal autenticado.
func (s messageSender) valid() bool {
	n := 0
	for _, id := range []string{s.authorID, s.webhookID, s.appInstallationID} {
		if id != "" {
			n++
		}
	}
	return n == 1
}

// owns exige autoria exclusiva, inclusive quando há dados históricos inconsistentes.
func (s messageSender) owns(m models.Message) bool {
	if !s.valid() {
		return false
	}
	if s.authorID != "" {
		return m.AuthorID != nil && *m.AuthorID == s.authorID && m.WebhookID == nil && m.AppInstallationID == nil
	}
	if s.webhookID != "" {
		return m.WebhookID != nil && *m.WebhookID == s.webhookID && m.AuthorID == nil && m.AppInstallationID == nil
	}
	return m.AppInstallationID != nil && *m.AppInstallationID == s.appInstallationID && m.AuthorID == nil && m.WebhookID == nil
}

// auditMetadata atribui a operação à máquina, nunca ao dono da mídia.
func (s messageSender) auditMetadata(channel string) map[string]any {
	if s.appInstallationID != "" {
		return map[string]any{"channel_id": channel, "app_installation_id": s.appInstallationID}
	}
	return map[string]any{"channel_id": channel, "webhook_id": s.webhookID}
}

// auditLabel usa o nome da instalação como snapshot opcional da auditoria.
func (s messageSender) auditLabel(ctx context.Context) string {
	if s.appInstallationID != "" {
		if a, err := activeApp(ctx, s.appInstallationID); err == nil {
			return "app:" + a.Name
		}
	}
	return ""
}
