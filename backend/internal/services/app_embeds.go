package services

import (
	"context"
	"papo/internal/models"
	"papo/internal/storage"
)

// declaredEmbed aceita exclusivamente superfície embed da própria instalação.
// As superfícies e origens do bundle já foram validadas no consentimento.
func declaredEmbed(ctx context.Context, id, surfaceID string) (AppSurfaceDeclaration, error) {
	b, err := ResolvePluginSurfaceBundle(ctx, id)
	if err != nil {
		return AppSurfaceDeclaration{}, err
	}
	return embedSurface(b, surfaceID)
}

// embedSurface procura a superfície embed surfaceID entre as aprovadas, com os
// mesmos padrões de validateAppSurfaces (load=click, display=inline) e a
// política de tamanho atual do servidor. Não refaz a validação das origens
// remotas: o backend nunca busca superfícies remotas, então resolver DNS a
// cada leitura não protegeria nada.
func embedSurface(b PluginSurfaceBundle, surfaceID string) (AppSurfaceDeclaration, error) {
	for _, s := range b.Surfaces {
		if s.ID != surfaceID || s.Kind != "embed" {
			continue
		}
		if s.Load == "" {
			s.Load = "click"
		}
		if s.Display == "" {
			s.Display = "inline"
		}
		if err := validateAppFramePolicy(s.Size, s.Load, s.Display); err != nil {
			return AppSurfaceDeclaration{}, err
		}
		return s, nil
	}
	return AppSurfaceDeclaration{}, ErrInvalidEmbed
}

// validateMessageEmbeds impede humanos/webhooks de criar superfícies e limita uma por mensagem.
func validateMessageEmbeds(ctx context.Context, appID string, inputs []EmbedInput) error {
	customs := make([]EmbedInput, 0, len(inputs))
	count := 0
	if len(inputs) > maxEmbedsPerMessage {
		return ErrTooManyEmbeds
	}
	for _, in := range inputs {
		if in.SourceType != "app" {
			customs = append(customs, in)
			continue
		}
		count++
		if count > 1 || appID == "" {
			return ErrInvalidEmbed
		}
		a, err := activeApp(ctx, appID)
		if err != nil {
			return err
		}
		if !a.Capabilities.PostAppEmbeds || !a.Capabilities.SendMessages {
			return ErrPermissionDenied
		}
		if in.URL != "" || in.Description != "" || in.Color != "" || in.SiteName != "" || in.Video != nil || in.Author != nil || in.Footer != nil || in.Image != nil || in.Thumbnail != nil || len(in.Fields) > 0 || in.AppSurfaceID == "" {
			return ErrInvalidEmbed
		}
		customs = append(customs, EmbedInput{Title: in.Title})
		if _, err := declaredEmbed(ctx, appID, in.AppSurfaceID); err != nil {
			return err
		}
	}
	return validateCustomEmbeds(customs)
}

// createMessageEmbeds mantém o fluxo customizado e grava referências de app separadamente.
func createMessageEmbeds(ctx context.Context, owner, appID string, inputs []EmbedInput) ([]models.Embed, error) {
	out := make([]models.Embed, 0, len(inputs))
	for _, in := range inputs {
		if in.SourceType != "app" {
			items, err := CreateCustomEmbeds(ctx, owner, []EmbedInput{in})
			if err != nil {
				return nil, err
			}
			out = append(out, items...)
			continue
		}
		s, err := declaredEmbed(ctx, appID, in.AppSurfaceID)
		if err != nil {
			return nil, err
		}
		prepared := models.Embed{SourceType: "app", FetchMethod: "manual", Title: nullableText(in.Title), AppInstallationID: &appID, AppSurfaceID: &s.ID,
			AppWidth: &s.Size.Width, AppHeight: &s.Size.Height, AppLoad: &s.Load, AppDisplay: &s.Display, AppPoster: nullableText(s.Poster)}
		e, err := storage.CreateAppEmbed(ctx, prepared)
		if err != nil {
			return nil, err
		}
		decorateAppEmbed(ctx, &e)
		out = append(out, e)
	}
	return out, nil
}

// decorateAppEmbed projeta o estado atual; histórico desativado não guarda URL executável.
func decorateAppEmbed(ctx context.Context, e *models.Embed) {
	if e.SourceType != "app" || e.AppInstallationID == nil || e.AppSurfaceID == nil {
		return
	}
	e.URL = nil
	e.EmbedURL = nil
	e.Entry = ""
	e.Poster = ""
	e.Surface = &models.AppSurfaceReference{AppInstallationID: *e.AppInstallationID, ID: *e.AppSurfaceID}
	if e.AppWidth != nil && e.AppHeight != nil {
		e.Size = &models.AppSurfaceSize{Width: *e.AppWidth, Height: *e.AppHeight}
	}
	if e.AppLoad != nil {
		e.Load = *e.AppLoad
	}
	if e.AppDisplay != nil {
		e.Display = *e.AppDisplay
	}
	if e.Size == nil || validateAppFramePolicy(*e.Size, e.Load, e.Display) != nil {
		return
	}
	b, err := ResolvePluginSurfaceBundle(ctx, *e.AppInstallationID)
	if err != nil {
		return
	}
	s, err := embedSurface(b, *e.AppSurfaceID)
	if err != nil {
		return
	}
	if e.AppPoster != nil {
		if err := validateAppPoster(b, *e.AppPoster); err != nil {
			return
		}
		e.Poster = pluginAssetURL(*e.AppInstallationID, b.Digest, *e.AppPoster)
	}
	e.Entry = s.Entry
	if s.Source == "bundled" {
		e.Entry = pluginAssetURL(*e.AppInstallationID, b.Digest, s.Entry)
	}
}

// CreatePluginMessage recebe identidade do contexto interno da PR 5, nunca de campos do usuário.
// O chamador publica efeitos de mensagem apenas quando created é true, como os handlers existentes.
func CreatePluginMessage(ctx context.Context, installationID, channelID, sourceID, content, replyTo string, attachments []AttachmentInput, embeds []EmbedInput) (models.MessageWithAttachment, bool, error) {
	a, err := activeApp(ctx, installationID)
	if err != nil {
		return models.MessageWithAttachment{}, false, err
	}
	if _, err := ResolvePluginSurfaceBundle(ctx, installationID); err != nil {
		return models.MessageWithAttachment{}, false, err
	}
	if !a.Capabilities.SendMessages {
		return models.MessageWithAttachment{}, false, ErrPermissionDenied
	}
	scoped, err := storage.AppHasScope(ctx, installationID, channelID)
	if err != nil {
		return models.MessageWithAttachment{}, false, err
	}
	if !scoped {
		return models.MessageWithAttachment{}, false, ErrPermissionDenied
	}
	if len(sourceID) > 200 {
		return models.MessageWithAttachment{}, false, ErrInvalidInput
	}
	name := a.Name
	if a.DisplayName != nil && *a.DisplayName != "" {
		name = *a.DisplayName
	}
	external := &models.ExternalAuthor{Source: "app", ID: a.ID, Name: name, AvatarURL: "/apps/" + a.ID + "/avatar"}
	return createMessageAs(ctx, channelID, messageSender{appInstallationID: a.ID}, external, sourceID, content, replyTo, attachments, embeds)
}
