package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
)

// ErrAppNotFound indica instalação inexistente ou revogada na gestão.
var ErrAppNotFound = errors.New("app não encontrado")

// ErrAppUnauthorized não revela se falhou o token, o tipo ou a revogação.
var ErrAppUnauthorized = errors.New("app não autorizado")

// validAppName valida nomes sem caracteres de controle.
func validAppName(name string, limit int) bool {
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > limit {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// newAppToken gera 32 bytes aleatórios e retorna somente seu hash para persistência.
func newAppToken() (string, string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", "", err
	}
	token := hex.EncodeToString(data)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

// CreateApp instala uma identidade do servidor; somente remote recebe token uma vez.
func CreateApp(ctx context.Context, actor, key, kind, name string, caps models.AppCapabilities) (models.AppInstallation, string, error) {
	if !validAppName(name, 80) || !validAppName(key, 200) || (kind != "remote" && kind != "plugin") {
		return models.AppInstallation{}, "", ErrInvalidInput
	}
	var hash *string
	token := ""
	if kind == "remote" {
		plain, hashed, err := newAppToken()
		if err != nil {
			return models.AppInstallation{}, "", err
		}
		token = plain
		hash = &hashed
	}
	a, err := storage.CreateApp(ctx, key, kind, strings.TrimSpace(name), actor, caps, hash)
	if err != nil {
		return a, "", err
	}
	RecordAudit(ctx, AuditEntry{ActorID: actor, Action: ActionAppCreate, EntityType: EntityApp, EntityID: &a.ID, Metadata: map[string]any{"name": a.Name, "kind": a.Kind}})
	return a, token, nil
}

// ListApps pagina todas as instalações ativas; gestão é independente do instalador.
func ListApps(ctx context.Context, since *time.Time, lastID, order string, limit int) (models.AppList, error) {
	if order != "" && order != "asc" && order != "desc" {
		return models.AppList{}, ErrInvalidInput
	}
	if since != nil && lastID != "" {
		if _, err := uuid.Parse(lastID); err != nil {
			return models.AppList{}, ErrInvalidInput
		}
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	apps, err := storage.ListApps(ctx, since, lastID, order == "asc", limit+1)
	if err != nil {
		return models.AppList{}, err
	}
	more := len(apps) > limit
	if more {
		apps = apps[:limit]
	}
	return models.AppList{Apps: apps, HasMore: more}, nil
}

// activeApp rejeita UUID inválido e instalações revogadas sem consultar o instalador.
func activeApp(ctx context.Context, id string) (models.AppInstallation, error) {
	if _, err := uuid.Parse(id); err != nil {
		return models.AppInstallation{}, ErrAppNotFound
	}
	a, err := storage.GetApp(ctx, id)
	if errors.Is(err, storage.ErrNotFound) || (err == nil && a.RevokedAt != nil) {
		return a, ErrAppNotFound
	}
	return a, err
}

// UpdateApp valida nome, capacidades e avatar preservando campos omitidos.
func UpdateApp(ctx context.Context, actorID, appID string, name *string, capabilities *models.AppCapabilities, displayName *string, avatar *string, avatarFormat string) (models.AppInstallation, error) {
	existing, err := activeApp(ctx, appID)
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, ErrAppNotFound) {
		return models.AppInstallation{}, ErrAppNotFound
	}
	if err != nil {
		return models.AppInstallation{}, err
	}
	// Instalação revogada é inerte: não se edita.
	if existing.RevokedAt != nil {
		return models.AppInstallation{}, ErrAppNotFound
	}

	var nameArg, displayNameArg, avatarArg any
	if name != nil {
		if !validAppName(*name, 80) {
			return models.AppInstallation{}, ErrInvalidInput
		}
		nameArg = strings.TrimSpace(*name)
	}
	if displayName != nil {
		trimmed := strings.TrimSpace(*displayName)
		if trimmed == "" {
			displayNameArg = sql.NullString{Valid: false}
		} else {
			if !validAppName(trimmed, maxExternalAuthorNameLength) {
				return models.AppInstallation{}, ErrInvalidInput
			}
			displayNameArg = sql.NullString{String: trimmed, Valid: true}
		}
	}

	if avatar != nil {
		if *avatar == "" {
			avatarArg = sql.NullString{Valid: false}
		} else {
			decoded, err := base64.StdEncoding.DecodeString(*avatar)
			if err != nil {
				return models.AppInstallation{}, ErrInvalidInput
			}
			upperFormat := normalizeImageFormat(avatarFormat)
			if !avatarContentMatchesFormat(decoded, upperFormat) || len(decoded) > maxAvatarBytes {
				return models.AppInstallation{}, ErrInvalidInput
			}
			if err := utils.ValidateImage(decoded, utils.MaxImageDimension); err != nil {
				return models.AppInstallation{}, ErrInvalidInput
			}
			sha, _, err := StoreMediaFromBytes(ctx, decoded, formatToMime(upperFormat))
			if err != nil {
				return models.AppInstallation{}, fmt.Errorf("falha ao gravar o avatar do app: %w", err)
			}
			avatarArg = sql.NullString{String: sha, Valid: true}
		}
	}

	app, err := storage.UpdateApp(ctx, appID, nameArg, displayNameArg, avatarArg, capabilities)
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, ErrAppNotFound) {
		return models.AppInstallation{}, ErrAppNotFound
	}
	if err != nil {
		return models.AppInstallation{}, err
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:    actorID,
		Action:     ActionAppUpdate,
		EntityType: EntityApp,
		EntityID:   &appID,
		Metadata:   map[string]any{"name": app.Name},
	})
	return app, nil
}

// GetAppAvatar resolve o avatar público inclusive após revogação.
func GetAppAvatar(ctx context.Context, id string) (models.Media, string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return models.Media{}, "", ErrAppNotFound
	}
	a, err := storage.GetApp(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return models.Media{}, "", ErrAppNotFound
	}
	if err != nil {
		return models.Media{}, "", err
	}
	if a.AvatarMedia == nil {
		return models.Media{}, "", ErrAvatarNotFound
	}
	return GetMedia(ctx, *a.AvatarMedia)
}

// RotateAppToken invalida imediatamente o token antigo e devolve o novo uma vez.
func RotateAppToken(ctx context.Context, actor, id string) (string, error) {
	a, err := activeApp(ctx, id)
	if err != nil {
		return "", err
	}
	if a.Kind != "remote" {
		return "", ErrInvalidInput
	}
	token, hash, err := newAppToken()
	if err != nil {
		return "", err
	}
	if err = storage.RotateAppToken(ctx, id, hash); errors.Is(err, storage.ErrNotFound) {
		return "", ErrAppNotFound
	} else if err != nil {
		return "", err
	}
	RecordAudit(ctx, AuditEntry{ActorID: actor, Action: ActionAppRotate, EntityType: EntityApp, EntityID: &id})
	return token, nil
}

// RevokeApp desativa a instalação sem apagar suas mensagens.
func RevokeApp(ctx context.Context, actor, id string) error {
	_, err := RevokeAppWithChannelChanges(ctx, actor, id)
	return err
}

// RevokeAppWithChannelChanges revoga e retorna os snapshots dos canais app removidos.
func RevokeAppWithChannelChanges(ctx context.Context, actor, id string) ([]AppChannelChange, error) {
	if _, err := activeApp(ctx, id); err != nil {
		return nil, err
	}
	channels, err := storage.RevokeAppWithChannelChanges(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, ErrAppNotFound
	}
	if err != nil {
		return nil, err
	}
	RecordAudit(ctx, AuditEntry{ActorID: actor, Action: ActionAppRevoke, EntityType: EntityApp, EntityID: &id})
	return channels, nil
}

// AddAppScope exige leitura humana do canal; a linha criada torna-se a ACL de máquina.
func AddAppScope(ctx context.Context, actor, id, channelID string) error {
	if _, err := activeApp(ctx, id); err != nil {
		return err
	}
	if _, err := uuid.Parse(channelID); err != nil {
		return ErrInvalidInput
	}
	ch, err := storage.GetChannelByID(ctx, channelID)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrChannelNotFound
	}
	if err != nil {
		return err
	}
	if ch.Type != "text" && ch.Type != "voice" {
		return ErrPermissionDenied
	}
	if err = requireWebhookChannelRead(ctx, ch, actor); err != nil {
		return err
	}
	if err = storage.AddAppScope(ctx, id, channelID, actor); errors.Is(err, storage.ErrNotFound) {
		return ErrAppNotFound
	} else if err != nil {
		return err
	}
	RecordAudit(ctx, AuditEntry{ActorID: actor, Action: ActionAppScopeAdd, EntityType: EntityApp, EntityID: &id, Metadata: map[string]any{"channel_id": channelID}})
	return nil
}

// RemoveAppScope retira acesso imediatamente sem exigir leitura do canal removido.
func RemoveAppScope(ctx context.Context, actor, id, channelID string) error {
	if _, err := activeApp(ctx, id); err != nil {
		return err
	}
	if _, err := uuid.Parse(channelID); err != nil {
		return ErrInvalidInput
	}
	if err := storage.RemoveAppScope(ctx, id, channelID); err != nil {
		return err
	}
	RecordAudit(ctx, AuditEntry{ActorID: actor, Action: ActionAppScopeRemove, EntityType: EntityApp, EntityID: &id, Metadata: map[string]any{"channel_id": channelID}})
	return nil
}

// AuthenticateApp aceita somente o segredo remoto e compara o hash em tempo constante.
func AuthenticateApp(ctx context.Context, token string) (models.AppInstallation, error) {
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	a, stored, err := storage.GetActiveAppByHash(ctx, hash)
	if err != nil || token == "" || subtle.ConstantTimeCompare([]byte(hash), []byte(stored)) != 1 {
		return models.AppInstallation{}, ErrAppUnauthorized
	}
	return a, nil
}

// authorizeApp recarrega capacidades e escopo em cada operação, sem herdar ACL humana.
func authorizeApp(ctx context.Context, a models.AppInstallation, channelID string, read bool) (models.AppInstallation, error) {
	current, err := activeApp(ctx, a.ID)
	if errors.Is(err, ErrAppNotFound) {
		return current, ErrAppUnauthorized
	}
	if err != nil {
		return current, err
	}
	if current.Kind != "remote" {
		return current, ErrAppUnauthorized
	}
	if (read && !current.Capabilities.ReadMessages) || (!read && !current.Capabilities.SendMessages) {
		return current, ErrPermissionDenied
	}
	if _, err := uuid.Parse(channelID); err != nil {
		return current, ErrInvalidInput
	}
	allowed, err := storage.AppHasScope(ctx, a.ID, channelID)
	if err != nil {
		return current, err
	}
	if !allowed {
		return current, ErrPermissionDenied
	}
	ch, err := storage.GetChannelByID(ctx, channelID)
	if errors.Is(err, storage.ErrNotFound) {
		return current, ErrChannelNotFound
	}
	if err != nil {
		return current, err
	}
	if ch.Type != "text" && ch.Type != "voice" {
		return current, ErrPermissionDenied
	}
	return current, nil
}

// CreateAppMessage envia como instalação; created_by é propriedade de armazenamento
// de attachments e embeds, não autoria. O snapshot nunca aceita override externo.
func CreateAppMessage(ctx context.Context, a models.AppInstallation, channelID, sourceID, content, replyTo string, attachments []AttachmentInput, embeds []EmbedInput) (models.MessageWithAttachment, bool, error) {
	a, err := authorizeApp(ctx, a, channelID, false)
	if err != nil {
		return models.MessageWithAttachment{}, false, err
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

// EditAppMessage verifica canal e posse antes da edição comum.
func EditAppMessage(ctx context.Context, a models.AppInstallation, channelID, messageID, content string, embeds []EmbedInput) (models.MessageWithAttachment, error) {
	if _, err := authorizeApp(ctx, a, channelID, false); err != nil {
		return models.MessageWithAttachment{}, err
	}
	if err := appOwnsMessage(ctx, a.ID, channelID, messageID); err != nil {
		return models.MessageWithAttachment{}, err
	}
	return editMessageAs(ctx, messageID, messageSender{appInstallationID: a.ID}, content, embeds)
}

// appOwnsMessage impede tocar mensagens de outro canal ou remetente.
func appOwnsMessage(ctx context.Context, id, channel, messageID string) error {
	if _, err := uuid.Parse(messageID); err != nil {
		return ErrMessageNotFound
	}
	m, err := storage.GetMessageByID(ctx, messageID)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrMessageNotFound
	}
	if err != nil {
		return err
	}
	if m.AppInstallationID == nil || *m.AppInstallationID != id || m.ChannelID != channel {
		return ErrPermissionDenied
	}
	return nil
}

// DeleteAppMessage exclui somente mensagem própria em canal ainda autorizado.
func DeleteAppMessage(ctx context.Context, a models.AppInstallation, channelID, messageID string) (string, error) {
	current, err := authorizeApp(ctx, a, channelID, false)
	if err != nil {
		return "", err
	}
	if err = appOwnsMessage(ctx, a.ID, channelID, messageID); err != nil {
		return "", err
	}
	if err = deleteMessage(ctx, messageID); err != nil {
		return "", err
	}
	RecordAudit(ctx, AuditEntry{ActorLabel: "app:" + current.Name, Action: ActionMessageDelete, EntityType: EntityMessage, EntityID: &messageID, Metadata: map[string]any{"channel_id": channelID, "app_installation_id": a.ID}})
	return channelID, nil
}

// ListAppMessages monta o histórico completo sem consultar reações de um usuário fictício.
func ListAppMessages(ctx context.Context, a models.AppInstallation, channelID string, since *time.Time, lastID, order string, limit int) (models.MessageList, error) {
	if _, err := authorizeApp(ctx, a, channelID, true); err != nil {
		return models.MessageList{}, err
	}
	if order != "" && order != "asc" && order != "desc" {
		return models.MessageList{}, ErrInvalidInput
	}
	if since != nil && lastID != "" {
		if _, err := uuid.Parse(lastID); err != nil {
			return models.MessageList{}, ErrInvalidInput
		}
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	items, err := storage.ListMessagesWithAttachmentsByChannelOrdered(ctx, channelID, since, lastID, order == "asc", limit)
	if err != nil {
		return models.MessageList{}, err
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	for i := range items {
		items[i], err = messageWithRelations(ctx, items[i].Message)
		if err != nil {
			return models.MessageList{}, err
		}
	}
	return models.MessageList{ChannelID: channelID, Messages: items, HasMore: more}, nil
}
