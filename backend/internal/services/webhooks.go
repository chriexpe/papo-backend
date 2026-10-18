package services

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"papo/internal/models"
	"papo/internal/storage"

	"github.com/google/uuid"
)

// ErrWebhookLimitReached indica que o canal atingiu o limite de webhooks ativos.
var ErrWebhookLimitReached = storage.ErrWebhookLimitReached

var ErrWebhookNotFound = errors.New("webhook não encontrado")
var ErrWebhookUnauthorized = errors.New("webhook não autorizado")
var ErrInvalidExternalAuthor = errors.New("autoria externa inválida")

// maxExternalAuthorNameLength limita o nickname da identidade do webhook
// (display_name) e o name de um external_author.
const maxExternalAuthorNameLength = 100

// webhookDisplayName resolve o nickname efetivo de exibição de um webhook:
// display_name quando definido (não vazio), senão o próprio name.
func webhookDisplayName(w models.Webhook) string {
	if w.DisplayName != nil && *w.DisplayName != "" {
		return *w.DisplayName
	}
	return w.Name
}

// webhookAvatarPath é o caminho público do avatar persistente de um webhook
// (GET /webhooks/:webhook_id/avatar, sem JWT). É um caminho relativo: o
// cliente resolve contra a origem do backend, sobrevive a mudanças de BaseURL.
func webhookAvatarPath(webhookID string) string {
	return "/webhooks/" + webhookID + "/avatar"
}

// CreateWebhook cria um webhook em um canal text ou voice e retorna o token de
// acesso em texto puro (a única vez em que ele existe fora do hash). O token é
// 32 bytes aleatórios em hex; a tabela guarda apenas o SHA-256, comparado em
// tempo constante na autenticação.
// Retorna ErrInvalidInput quando o nome é vazio ou excede 80 caracteres,
// ErrChannelNotFound quando o canal não existe e ErrPermissionDenied quando o
// canal não aceita webhook (dm) ou o ator não pode ler o canal.
// ErrWebhookLimitReached indica que o canal tem 50 webhooks ativos.
func CreateWebhook(ctx context.Context, actorID, channelID, name string) (models.Webhook, string, error) {
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 80 {
		return models.Webhook{}, "", ErrInvalidInput
	}

	channel, err := storage.GetChannelByID(ctx, channelID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.Webhook{}, "", ErrChannelNotFound
	}
	if err != nil {
		return models.Webhook{}, "", err
	}
	if channel.Type != "text" && channel.Type != "voice" {
		return models.Webhook{}, "", ErrPermissionDenied
	}

	if err := requireWebhookChannelRead(ctx, channel, actorID); err != nil {
		return models.Webhook{}, "", err
	}

	secret, hash, err := newMachineToken()
	if err != nil {
		return models.Webhook{}, "", err
	}

	webhook, err := storage.CreateWebhook(ctx, channelID, name, actorID, hash)
	if err != nil {
		return models.Webhook{}, "", err
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:    actorID,
		Action:     ActionWebhookCreate,
		EntityType: EntityWebhook,
		EntityID:   &webhook.ID,
		Metadata:   map[string]any{"channel_id": channelID, "name": name},
	})
	return webhook, secret, nil
}

// MaxWebhookListLimit é o teto de itens por página, como na listagem de mensagens.
const MaxWebhookListLimit = 100

// ListWebhooks pagina somente webhooks de canais que o ator pode ler.
// Segue mensagens: since/last_id, order desc por padrão e limit 30 (máximo 100).
func ListWebhooks(ctx context.Context, actorID string, since *time.Time, lastID, order string, limit int) (models.WebhookList, error) {
	orderAsc := false
	switch order {
	case "", "desc":
	case "asc":
		orderAsc = true
	default:
		return models.WebhookList{}, ErrInvalidInput
	}
	if since != nil && lastID != "" {
		if _, err := uuid.Parse(lastID); err != nil {
			return models.WebhookList{}, ErrInvalidInput
		}
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > MaxWebhookListLimit {
		limit = MaxWebhookListLimit
	}
	items, err := storage.ListWebhooks(ctx, actorID, since, lastID, orderAsc, limit+1)
	if err != nil {
		return models.WebhookList{}, err
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	return models.WebhookList{Webhooks: items, HasMore: hasMore}, nil
}

// requireWebhookChannelRead aplica a mesma ACL de leitura usada por mensagens.
func requireWebhookChannelRead(ctx context.Context, channel models.Channel, actorID string) error {
	allowed, err := userHasChannelPermission(ctx, channel, actorID, true, func(p models.ChannelPermission) bool { return p.ReadChannel })
	if err != nil {
		return err
	}
	if !allowed {
		return ErrPermissionDenied
	}
	return nil
}

// requireWebhookRead carrega o canal do webhook e exige leitura pelo ator.
func requireWebhookRead(ctx context.Context, channelID, actorID string) error {
	channel, err := storage.GetChannelByID(ctx, channelID)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrChannelNotFound
	}
	if err != nil {
		return err
	}
	return requireWebhookChannelRead(ctx, channel, actorID)
}

// GetWebhook resolve um webhook pelo id, incluindo revogados.
func GetWebhook(ctx context.Context, id string) (models.Webhook, error) {
	w, err := storage.GetWebhookByID(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return models.Webhook{}, ErrWebhookNotFound
	}
	return w, err
}

// UpdateWebhookIdentity atualiza a identidade persistente de um webhook
// (nickname e/ou avatar). Campos ausentes preservam o valor atual:
//   - displayName: NULL = preservar; "" = remover o nickname (volta a usar o
//     name do webhook); otherwise validar (não vazio, até 100 caracteres).
//   - avatar/avatarFormat: avatar nil = preservar; avatar "" = remover o
//     avatar; otherwise base64 de um GIF, JPEG, PNG ou WEBP de até 2MB com
//     dimensões de até 512px (mesmas regras do avatar de usuário), gravado no
//     storage content-addressable.
//
// Retorna ErrWebhookNotFound quando o webhook não existe (inclusive revogado)
// e ErrInvalidInput quando um campo viola as regras. ErrPermissionDenied indica
// que o ator não pode ler o canal do webhook.
func UpdateWebhookIdentity(ctx context.Context, actorID, webhookID string, displayName *string, avatar *string, avatarFormat string) (models.Webhook, error) {
	existing, err := storage.GetWebhookByID(ctx, webhookID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.Webhook{}, ErrWebhookNotFound
	}
	if err != nil {
		return models.Webhook{}, err
	}
	// Webhook revogado é credencial inerte: não se edita.
	if existing.RevokedAt != nil {
		return models.Webhook{}, ErrWebhookNotFound
	}

	if err := requireWebhookRead(ctx, existing.ChannelID, actorID); err != nil {
		return models.Webhook{}, err
	}

	var displayNameArg any
	if displayName != nil {
		trimmed := strings.TrimSpace(*displayName)
		if trimmed == "" {
			displayNameArg = sql.NullString{Valid: false}
		} else {
			if utf8.RuneCountInString(trimmed) > maxExternalAuthorNameLength {
				return models.Webhook{}, ErrInvalidInput
			}
			displayNameArg = sql.NullString{String: trimmed, Valid: true}
		}
	}

	avatarArg, err := machineAvatarArg(ctx, avatar, avatarFormat, "webhook")
	if err != nil {
		return models.Webhook{}, err
	}

	webhook, err := storage.UpdateWebhookIdentity(ctx, webhookID, displayNameArg, avatarArg)
	if errors.Is(err, storage.ErrNotFound) {
		return models.Webhook{}, ErrWebhookNotFound
	}
	if err != nil {
		return models.Webhook{}, err
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:    actorID,
		Action:     ActionWebhookUpdate,
		EntityType: EntityWebhook,
		EntityID:   &webhookID,
		Metadata:   map[string]any{"channel_id": webhook.ChannelID, "name": webhook.Name},
	})
	return webhook, nil
}

// GetWebhookAvatar resolve o avatar persistente de um webhook para servir os
// bytes: retorna o registro media (MIME para o Content-Type) e o caminho do
// blob no disco. Retorna ErrWebhookNotFound quando o webhook não existe,
// ErrAvatarNotFound quando ele não tem avatar e ErrMediaNotFound quando o
// blob sumiu do disco.
func GetWebhookAvatar(ctx context.Context, webhookID string) (models.Media, string, error) {
	sha, err := storage.GetWebhookAvatarMedia(ctx, webhookID)
	if errors.Is(err, storage.ErrNotFound) {
		return models.Media{}, "", ErrWebhookNotFound
	}
	if err != nil {
		return models.Media{}, "", err
	}
	if sha == nil || *sha == "" {
		return models.Media{}, "", ErrAvatarNotFound
	}

	return GetMedia(ctx, *sha)
}

// RevokeWebhook revoga um webhook (soft delete): o token deixa de autenticar,
// mas as mensagens já enviadas mantêm a atribuição. Retorna ErrWebhookNotFound
// quando o webhook não existe ou já está revogado; exige leitura do canal.
func RevokeWebhook(ctx context.Context, actorID, id string) error {
	w, err := storage.GetWebhookByID(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrWebhookNotFound
	}
	if err != nil {
		return err
	}

	if w.RevokedAt != nil {
		return ErrWebhookNotFound
	}
	if err := requireWebhookRead(ctx, w.ChannelID, actorID); err != nil {
		return err
	}

	if err := storage.RevokeWebhook(ctx, id); errors.Is(err, storage.ErrNotFound) {
		return ErrWebhookNotFound
	} else if err != nil {
		return err
	}

	RecordAudit(ctx, AuditEntry{
		ActorID:    actorID,
		Action:     ActionWebhookRevoke,
		EntityType: EntityWebhook,
		EntityID:   &id,
		Metadata:   map[string]any{"channel_id": w.ChannelID, "name": w.Name},
	})
	return nil
}

// AuthenticateWebhook valida o token de acesso de um webhook: o SHA-256 do
// token informado é comparado em tempo constante com o hash armazenado.
// Webhook inexistente, revogado ou token errado retornam a mesma
// ErrWebhookUnauthorized (não revela qual falhou).
func AuthenticateWebhook(ctx context.Context, id, token string) (models.Webhook, error) {
	w, hash, err := storage.GetActiveWebhookWithHash(ctx, id)
	if err != nil {
		return models.Webhook{}, ErrWebhookUnauthorized
	}
	if subtle.ConstantTimeCompare([]byte(machineTokenHash(token)), []byte(hash)) != 1 {
		return models.Webhook{}, ErrWebhookUnauthorized
	}
	return w, nil
}

// resolveWebhookAuthor monta a autoria de exibição de uma mensagem de webhook.
// external nil usa a identidade persistente do webhook (display_name ou name,
// mais o avatar quando existe, referenciado pelo caminho público do webhook).
// Quando external é informada (atribuição temporária da mensagem), ela vale
// integralmente; um avatar_url ausente é preenchido com o avatar persistente
// do webhook, quando houver.
func resolveWebhookAuthor(w models.Webhook, external *models.ExternalAuthor) *models.ExternalAuthor {
	if external == nil {
		author := &models.ExternalAuthor{
			Source: "webhook",
			ID:     w.ID,
			Name:   webhookDisplayName(w),
		}
		if w.AvatarMedia != nil && *w.AvatarMedia != "" {
			author.AvatarURL = webhookAvatarPath(w.ID)
		}
		return author
	}
	if external.AvatarURL == "" && w.AvatarMedia != nil && *w.AvatarMedia != "" {
		external.AvatarURL = webhookAvatarPath(w.ID)
	}
	return external
}

// validateExternalAuthor valida a autoria externa informada no envio.
// Source é um identificador de máquina (minúsculas, dígitos, '_' e '-'), Name
// não aceita caracteres de controle, e as URLs passam pelo mesmo validador de
// embed (bloqueia hosts locais/privados). Limites seguem models.ExternalAuthor.
func validateExternalAuthor(a *models.ExternalAuthor) error {
	if a == nil {
		return nil
	}
	if strings.TrimSpace(a.Source) == "" || len(a.Source) > 40 ||
		strings.TrimSpace(a.ID) == "" || len(a.ID) > 200 ||
		strings.TrimSpace(a.Name) == "" || utf8.RuneCountInString(a.Name) > maxExternalAuthorNameLength {
		return ErrInvalidExternalAuthor
	}
	for _, ch := range a.Source {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return ErrInvalidExternalAuthor
		}
	}
	for _, ch := range a.Name {
		if ch < 0x20 || ch == 0x7f {
			return ErrInvalidExternalAuthor
		}
	}
	if a.AvatarURL != "" {
		if _, err := validateEmbedURL(a.AvatarURL); err != nil {
			return ErrInvalidExternalAuthor
		}
	}
	if a.ProfileURL != "" {
		if _, err := validateEmbedURL(a.ProfileURL); err != nil {
			return ErrInvalidExternalAuthor
		}
	}
	return nil
}
