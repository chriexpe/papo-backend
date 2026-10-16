package services

import (
	"context"
	"papo/internal/models"
	"papo/internal/storage"
)

// appSurfacePayload resolve a URL bundled sob o caminho público permitido pelo contrato.
func appSurfacePayload(id string, s AppSurfaceDeclaration) *models.AppChannel {
	entry := s.Entry
	if s.Source == "bundled" {
		entry = "/apps/" + id + "/" + entry
	}
	return &models.AppChannel{AppInstallationID: id, SurfaceID: s.ID, Source: s.Source, Entry: entry, AvatarURL: "/apps/" + id + "/avatar", Available: true}
}

// declaredEmbed aceita exclusivamente superfície embed da própria instalação e origem aprovada.
func declaredEmbed(ctx context.Context, id, surfaceID string) (AppSurfaceDeclaration, error) {
	b, err := ResolvePluginSurfaceBundle(ctx, id)
	if err != nil {
		return AppSurfaceDeclaration{}, err
	}
	validated, err := validateAppSurfaces(ctx, b, b.Surfaces)
	if err != nil {
		return AppSurfaceDeclaration{}, err
	}
	for _, s := range validated {
		if s.ID == surfaceID && s.Kind == "embed" {
			return s, nil
		}
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
		if in.URL != "" || in.Video != nil || in.Author != nil || in.AppSurfaceID == "" {
			return ErrInvalidEmbed
		}
		copy := in
		copy.SourceType = ""
		copy.AppSurfaceID = ""
		customs = append(customs, copy)
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
		copy := in
		copy.SourceType = ""
		copy.AppSurfaceID = ""
		prepared, fields := buildCustomEmbed(copy)
		prepared.SourceType = "app"
		prepared.AppInstallationID = &appID
		prepared.AppSurfaceID = &s.ID
		prepared.FrameWidth = &s.FrameWidth
		prepared.FrameHeight = &s.FrameHeight
		prepared.Fields = fields
		resolveCustomEmbedMedia(ctx, owner, &prepared)
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
	e.App = &models.AppChannel{AppInstallationID: *e.AppInstallationID, SurfaceID: *e.AppSurfaceID, AvatarURL: "/apps/" + *e.AppInstallationID + "/avatar", Available: false}
	s, err := declaredEmbed(ctx, *e.AppInstallationID, *e.AppSurfaceID)
	if err != nil {
		return
	}
	e.App = appSurfacePayload(*e.AppInstallationID, s)
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
