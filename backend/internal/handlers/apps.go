package handlers

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/utils"
	"papo/internal/websocket"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// createAppRequest é o corpo de POST /apps.
type createAppRequest struct {
	AppKey       string                 `json:"app_key"`
	Kind         string                 `json:"kind"`
	Capabilities models.AppCapabilities `json:"capabilities"`
	Name         string                 `json:"name"`
}

// createAppResponse é a resposta de POST /apps: o token em texto puro
// aparece somente aqui (a única vez em que existe fora do hash armazenado).
type createAppResponse struct {
	models.AppInstallation
	Token string `json:"token,omitempty"`
}

// appEditRequest é o corpo de PUT /app/channels/:channel_id/messages/:message_id.
// Embeds ausentes preservam os embeds existentes (edição apenas de texto),
// como na edição de mensagem humana.
type appEditRequest struct {
	Content string                `json:"content"`
	Embeds  []services.EmbedInput `json:"embeds"`
}

// appIdentityRequest é o corpo de PATCH /apps/:app_id (identidade
// persistente). Campos ausentes preservam o valor atual; avatar "" remove o
// avatar; display_name "" remove o nickname (volta a usar o name).
type appIdentityRequest struct {
	Name         *string                 `json:"name"`
	Capabilities *models.AppCapabilities `json:"capabilities"`
	DisplayName  *string                 `json:"display_name"`
	Avatar       *string                 `json:"avatar"`
	AvatarFormat string                  `json:"avatar_format"`
}

// CreateAppHandler implementa POST /apps (JWT + manage_apps):
// instala uma identidade do servidor; somente remote recebe token.
func CreateAppHandler(baseURL string, c echo.Context) error {
	actor, _ := c.Get(middleware.UserIDContextKey).(string)

	var req createAppRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "JSON inválido")
	}

	if req.Kind != "remote" {
		return appProblem(c, baseURL, services.ErrInvalidInput)
	}
	app, token, err := services.CreateApp(c.Request().Context(), actor, req.AppKey, req.Kind, req.Name, req.Capabilities)
	if err != nil {
		return appProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusCreated, createAppResponse{AppInstallation: app, Token: token})
}

// ListAppsHandler lista instalações ativas do servidor, com a paginação de
// mensagens: since, last_id, order e limit; responde apps e has_more.
func ListAppsHandler(baseURL string, c echo.Context) error {
	var since *time.Time
	if value := c.QueryParam("since"); value != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return utils.SendProblem(c, baseURL, http.StatusBadRequest,
				"invalid-param", "Parâmetro inválido", "since deve ser um timestamp ISO 8601")
		}
		since = &parsed
	}
	limit := 0
	if value := c.QueryParam("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > services.MaxMessageListLimit {
			return utils.SendProblem(c, baseURL, http.StatusBadRequest,
				"invalid-param", "Parâmetro inválido", "limit deve ser um inteiro entre 1 e 100")
		}
		limit = parsed
	}
	items, err := services.ListApps(c.Request().Context(), since, c.QueryParam("last_id"), c.QueryParam("order"), limit)
	if err != nil {
		return appProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusOK, items)
}

// UpdateAppHandler implementa PATCH /apps/:app_id (JWT +
// manage_apps): atualiza a identidade persistente do app (nickname e
// avatar base64, mesmas regras do avatar de usuário).
func UpdateAppHandler(baseURL string, c echo.Context) error {
	actor, _ := c.Get(middleware.UserIDContextKey).(string)

	var req appIdentityRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "JSON inválido")
	}

	app, err := services.UpdateApp(c.Request().Context(), actor, c.Param("app_id"),
		req.Name, req.Capabilities, req.DisplayName, req.Avatar, req.AvatarFormat)
	if err != nil {
		return appProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusOK, app)
}

// GetAppAvatarHandler implementa GET /apps/:app_id/avatar (público,
// sem JWT): serve o avatar persistente do app a partir do storage
// content-addressable, como GET /users/:user_id/avatar. Consumidores externos
// usam essa URL absoluta sem cookie de sessão; quando o app não tem
// avatar, 404.
func GetAppAvatarHandler(baseURL string, c echo.Context) error {
	appID := c.Param("app_id")
	if _, err := uuid.Parse(appID); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "app não encontrado")
	}

	media, filePath, err := services.GetAppAvatar(c.Request().Context(), appID)
	switch {
	case errors.Is(err, services.ErrAppNotFound),
		errors.Is(err, services.ErrAvatarNotFound),
		errors.Is(err, services.ErrMediaNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "avatar não encontrado")
	case err != nil:
		utils.Errorf("request_id=%s falha ao buscar o avatar do app: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao buscar o avatar do app")
	}

	file, err := os.Open(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Recurso não encontrado", "avatar não encontrado")
	}
	if err != nil {
		utils.Errorf("request_id=%s falha ao abrir o avatar do app: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao buscar o avatar do app")
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		utils.Errorf("request_id=%s falha ao ler o avatar do app: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha ao buscar o avatar do app")
	}

	resp := c.Response()
	resp.Header().Set(echo.HeaderContentType, media.MimeType)
	resp.Header().Set(echo.HeaderContentDisposition, "inline")
	// A URL é por app_id (não por sha), então o cache precisa ser curto o
	// suficiente para refletir trocas de avatar.
	resp.Header().Set("Cache-Control", "public, max-age=3600")

	// ServeContent trata Range requests e If-Modified-Since (304).
	http.ServeContent(resp, c.Request(), appID, info.ModTime(), file)

	return nil
}

// DeleteAppHandler implementa DELETE /apps/:app_id (JWT +
// manage_apps): revoga o app (soft delete). As mensagens já enviadas
// mantêm a atribuição.
func DeleteAppHandler(baseURL string, c echo.Context) error {
	actor, _ := c.Get(middleware.UserIDContextKey).(string)
	channels, err := services.RevokeAppWithChannelChanges(c.Request().Context(), actor, c.Param("app_id"))
	if err != nil {
		return appProblem(c, baseURL, err)
	}
	BroadcastAppChannelChanges(c.Request().Context(), channels)
	return c.NoContent(http.StatusNoContent)
}

// appProblem mapeia os erros dos services de app para RFC 7807, com os
// mesmos status do fluxo equivalente de mensagens humanas.
func appProblem(c echo.Context, baseURL string, err error) error {
	switch {
	case errors.Is(err, services.ErrAppUnauthorized):
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized,
			"unauthorized", "Não autorizado", "app inválido")
	case errors.Is(err, services.ErrAppNotFound),
		errors.Is(err, services.ErrChannelNotFound),
		errors.Is(err, services.ErrMessageNotFound):
		return utils.SendProblem(c, baseURL, http.StatusNotFound,
			"not-found", "Não encontrado", "recurso inexistente")
	case errors.Is(err, services.ErrPermissionDenied),
		errors.Is(err, services.ErrDirectMessageBlocked):
		return utils.SendProblem(c, baseURL, http.StatusForbidden,
			"forbidden", "Acesso negado", "operação não permitida")
	case errors.Is(err, services.ErrInvalidInput),
		errors.Is(err, services.ErrInvalidEmbed),
		errors.Is(err, services.ErrInvalidExternalAuthor),
		errors.Is(err, services.ErrTooManyAttachments),
		errors.Is(err, services.ErrTooManyEmbeds),
		errors.Is(err, services.ErrAttachmentTooLarge):
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", err.Error())
	default:
		utils.Errorf("request_id=%s falha em operação de app: %v",
			c.Request().Header.Get(echo.HeaderXRequestID), err)
		return utils.SendProblem(c, baseURL, http.StatusInternalServerError,
			"internal", "Erro interno", "falha no app")
	}
}

// appFromRequest recupera a instalação validada pelo middleware Bearer.
func appFromRequest(baseURL string, c echo.Context) (models.AppInstallation, error) {
	a, ok := c.Get("app_installation").(models.AppInstallation)
	if !ok {
		return a, services.ErrAppUnauthorized
	}
	return a, nil
}

// ExecuteAppHandler implementa POST /app/channels/:channel_id/messages:
// envio de mensagem com a credencial escopada do app (Bearer do token,
// nunca JWT de usuário). multipart/form-data com content, embeds (JSON),
// external_message_id (chave de
// idempotência opcional), reply_to e attachments.
//
// Compartilha com mensagens humanas todo o fluxo pós-criação (WS message,
// embeds, moderação, notificações). Retry com a mesma external_message_id
// devolve a mensagem existente com 200, sem reprocessar side effects.
func ExecuteAppHandler(baseURL string, c echo.Context) error {
	app, err := appFromRequest(baseURL, c)
	if err != nil {
		return appProblem(c, baseURL, err)
	}

	if err := c.Request().ParseMultipartForm(110 << 20); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido",
			"corpo da requisição deve ser multipart/form-data válido")
	}

	content := c.FormValue("content")
	sourceID := c.FormValue("external_message_id")
	replyTo := c.FormValue("reply_to")

	if c.FormValue("external_author") != "" {
		return appProblem(c, baseURL, services.ErrInvalidInput)
	}

	embeds, err := parseEmbedsField(c.FormValue("embeds"))
	if err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "campo embeds deve ser um array JSON de embeds")
	}

	var attachments []services.AttachmentInput
	if c.Request().MultipartForm != nil {
		for _, fileHeader := range c.Request().MultipartForm.File["attachments"] {
			file, err := fileHeader.Open()
			if err != nil {
				return utils.SendProblem(c, baseURL, http.StatusBadRequest,
					"invalid-param", "Parâmetro inválido", "falha ao ler o attachment enviado")
			}
			defer file.Close()
			attachments = append(attachments, services.AttachmentInput{
				OriginalFileName: fileHeader.Filename,
				Content:          file,
			})
		}
	}

	message, created, err := services.CreateAppMessage(c.Request().Context(), app, c.Param("channel_id"),
		sourceID, content, replyTo, attachments, embeds)
	if err != nil {
		return appProblem(c, baseURL, err)
	}

	// Retry idempotente: a mensagem original já distribuiu seus eventos; não
	// reprocessar broadcast, moderação ou notificações.
	if created {
		publishCreatedMessage(c, message, app.CreatedBy)
	}

	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	return c.JSON(status, message)
}

// EditAppMessageHandler implementa PUT /app/channels/:channel_id/messages/:message_id:
// somente o app autor pode editar mensagem que ele mesmo enviou
// (verificação no service). O fluxo de eventos é idêntico ao da edição humana.
func EditAppMessageHandler(baseURL string, c echo.Context) error {
	app, err := appFromRequest(baseURL, c)
	if err != nil {
		return appProblem(c, baseURL, err)
	}

	var req appEditRequest
	if err := c.Bind(&req); err != nil {
		return utils.SendProblem(c, baseURL, http.StatusBadRequest,
			"invalid-param", "Parâmetro inválido", "JSON inválido")
	}

	message, err := services.EditAppMessage(c.Request().Context(), app, c.Param("channel_id"), c.Param("message_id"), req.Content, req.Embeds)
	if err != nil {
		return appProblem(c, baseURL, err)
	}

	broadcastChannelEvent(c, message.ChannelID, websocket.MessageEditOutbound{
		Type:      websocket.EventTypeMessageEdit,
		ID:        message.ID,
		ChannelID: message.ChannelID,
		Content:   derefString(message.Content),
		EditedAt:  derefTime(message.EditedAt),
	})
	requestID := c.Request().Header.Get(echo.HeaderXRequestID)
	broadcastMessageEmbedsUpdate(c.Request().Context(), requestID, message.ChannelID, message.ID)
	go processEditedMessageEmbeds(context.Background(), requestID, message.ChannelID, message.ID,
		app.CreatedBy, derefString(message.Content), message.Embeds)

	return c.JSON(http.StatusOK, message)
}

// DeleteAppMessageHandler implementa DELETE /app/channels/:channel_id/messages/:message_id:
// somente o app autor pode excluir mensagem que ele mesmo enviou.
func DeleteAppMessageHandler(baseURL string, c echo.Context) error {
	app, err := appFromRequest(baseURL, c)
	if err != nil {
		return appProblem(c, baseURL, err)
	}

	channelID, err := services.DeleteAppMessage(c.Request().Context(), app, c.Param("channel_id"), c.Param("message_id"))
	if err != nil {
		return appProblem(c, baseURL, err)
	}

	broadcastChannelEvent(c, channelID, websocket.MessageDeleteOutbound{
		Type:      websocket.EventTypeMessageDelete,
		ID:        c.Param("message_id"),
		ChannelID: channelID,
	})
	return c.NoContent(http.StatusNoContent)
}

// RotateAppTokenHandler devolve o novo segredo uma única vez.
func RotateAppTokenHandler(baseURL string, c echo.Context) error {
	actor, _ := c.Get(middleware.UserIDContextKey).(string)
	token, err := services.RotateAppToken(c.Request().Context(), actor, c.Param("app_id"))
	if err != nil {
		return appProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusOK, map[string]string{"token": token})
}

// AddAppScopeHandler concede acesso ao canal legível pelo ator humano.
func AddAppScopeHandler(baseURL string, c echo.Context) error {
	actor, _ := c.Get(middleware.UserIDContextKey).(string)
	if err := services.AddAppScope(c.Request().Context(), actor, c.Param("app_id"), c.Param("channel_id")); err != nil {
		return appProblem(c, baseURL, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// RemoveAppScopeHandler retira a autorização de máquina para o canal.
func RemoveAppScopeHandler(baseURL string, c echo.Context) error {
	actor, _ := c.Get(middleware.UserIDContextKey).(string)
	if err := services.RemoveAppScope(c.Request().Context(), actor, c.Param("app_id"), c.Param("channel_id")); err != nil {
		return appProblem(c, baseURL, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// ListAppMessagesHandler lê histórico paginado, incluindo attachments, embeds e reações.
func ListAppMessagesHandler(baseURL string, c echo.Context) error {
	a, err := appFromRequest(baseURL, c)
	if err != nil {
		return appProblem(c, baseURL, err)
	}
	var since *time.Time
	if value := c.QueryParam("since"); value != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return appProblem(c, baseURL, services.ErrInvalidInput)
		}
		since = &parsed
	}
	limit := 0
	if value := c.QueryParam("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 100 {
			return appProblem(c, baseURL, services.ErrInvalidInput)
		}
		limit = parsed
	}
	page, err := services.ListAppMessages(c.Request().Context(), a, c.Param("channel_id"), since, c.QueryParam("last_id"), c.QueryParam("order"), limit)
	if err != nil {
		return appProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusOK, page)
}
