package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/plugins"
	"papo/internal/services"
	"papo/internal/storage"
	"papo/internal/utils"
	"papo/internal/websocket"
)

// pluginRequestTimeout limita cada operação de gestão de plugins.
const pluginRequestTimeout = 10 * time.Second

// pluginOptions limita seleção administrativa e rejeita campos desconhecidos.
func pluginOptions(c echo.Context) (services.PluginOptions, error) {
	var o services.PluginOptions
	d := json.NewDecoder(io.LimitReader(c.Request().Body, plugins.MaxPayload+1))
	d.DisallowUnknownFields()
	if err := d.Decode(&o); err != nil {
		return o, services.ErrInvalidInput
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return o, services.ErrInvalidInput
	}
	return o, nil
}

// pluginContext aplica o prazo de gestão e devolve o ator autenticado.
func pluginContext(c echo.Context) (context.Context, context.CancelFunc, string) {
	ctx, cancel := context.WithTimeout(c.Request().Context(), pluginRequestTimeout)
	actor, _ := c.Get(middleware.UserIDContextKey).(string)
	return ctx, cancel, actor
}

// ListPluginsHandler implementa GET /plugins: catálogo descoberto e estado de
// cada instalação. Exige manage_apps.
func ListPluginsHandler(baseURL string, m *services.PluginManager, c echo.Context) error {
	ctx, cancel, _ := pluginContext(c)
	defer cancel()
	list, err := m.List(ctx)
	if err != nil {
		return pluginProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"plugins": list})
}

// RescanPluginsHandler implementa POST /plugins/rescan: redescobre PLUGINS_DIR,
// reconcilia as instalações e devolve o catálogo atualizado.
func RescanPluginsHandler(baseURL string, m *services.PluginManager, c echo.Context) error {
	ctx, cancel, _ := pluginContext(c)
	defer cancel()
	if err := m.Rescan(ctx); err != nil {
		return pluginProblem(c, baseURL, err)
	}
	list, err := m.List(ctx)
	if err != nil {
		return pluginProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"plugins": list})
}

// PreviewPluginHandler implementa POST /plugins/:id/preview: monta e assina o
// consentimento da seleção enviada, sem efeitos.
func PreviewPluginHandler(baseURL string, m *services.PluginManager, c echo.Context) error {
	ctx, cancel, actor := pluginContext(c)
	defer cancel()
	o, err := pluginOptions(c)
	if err != nil {
		return appProblem(c, baseURL, err)
	}
	p, err := m.Preview(ctx, actor, c.Param("id"), o)
	if err != nil {
		return pluginProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusOK, p)
}

// EnablePluginHandler implementa POST /plugins/:id/enable com a mesma seleção
// e o consent do preview.
func EnablePluginHandler(baseURL string, m *services.PluginManager, c echo.Context) error {
	ctx, cancel, actor := pluginContext(c)
	defer cancel()
	o, err := pluginOptions(c)
	if err != nil {
		return appProblem(c, baseURL, err)
	}
	a, err := m.Enable(ctx, actor, c.Param("id"), o)
	if err != nil {
		return pluginProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusOK, a)
}

// PatchPluginHandler implementa PATCH /plugins/:id: settings, memória e quotas
// da versão aprovada.
func PatchPluginHandler(baseURL string, m *services.PluginManager, c echo.Context) error {
	ctx, cancel, actor := pluginContext(c)
	defer cancel()
	o, err := pluginOptions(c)
	if err != nil {
		return appProblem(c, baseURL, err)
	}
	a, err := m.Patch(ctx, actor, c.Param("id"), o)
	if err != nil {
		return pluginProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusOK, a)
}

// DisablePluginHandler implementa POST /plugins/:id/disable.
func DisablePluginHandler(baseURL string, m *services.PluginManager, c echo.Context) error {
	ctx, cancel, actor := pluginContext(c)
	defer cancel()
	if err := m.Disable(ctx, actor, c.Param("id")); err != nil {
		return pluginProblem(c, baseURL, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// TriggerPluginHandler implementa POST /plugins/:id/trigger: entrega o JSON do
// corpo ao on_trigger e devolve a resposta do plugin.
func TriggerPluginHandler(baseURL string, m *services.PluginManager, c echo.Context) error {
	ctx, cancel, _ := pluginContext(c)
	defer cancel()
	data, err := io.ReadAll(io.LimitReader(c.Request().Body, plugins.MaxPayload+1))
	if err != nil || len(data) > plugins.MaxPayload || !json.Valid(data) {
		return appProblem(c, baseURL, services.ErrInvalidInput)
	}
	out, err := m.Trigger(ctx, c.Param("id"), data)
	if err != nil {
		return pluginProblem(c, baseURL, err)
	}
	return c.JSONBlob(http.StatusOK, out)
}

// pluginProblem mantém erros de admissão/execução fora do envelope do guest.
func pluginProblem(c echo.Context, baseURL string, err error) error {
	switch {
	case errors.Is(err, services.ErrPluginConsent):
		return utils.SendProblem(c, baseURL, http.StatusConflict, "conflict", "Novo consentimento necessário", err.Error())
	case errors.Is(err, plugins.ErrInvalidArtifact):
		return appProblem(c, baseURL, services.ErrInvalidInput)
	case errors.Is(err, plugins.ErrBusy):
		return utils.SendProblem(c, baseURL, http.StatusTooManyRequests, "rate-limit", "Limite de plugins", "fila ou cache ocupado")
	case errors.Is(err, plugins.ErrQuarantined):
		return utils.SendProblem(c, baseURL, http.StatusConflict, "conflict", "Plugin em quarentena", err.Error())
	case errors.Is(err, plugins.ErrExecution), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return utils.SendProblem(c, baseURL, http.StatusUnprocessableEntity, "plugin-execution", "Falha no plugin",
			"execução interrompida ou resposta inválida")
	case errors.Is(err, storage.ErrNotFound):
		return appProblem(c, baseURL, services.ErrAppNotFound)
	default:
		return appProblem(c, baseURL, err)
	}
}

// PluginEffects publica pelo hub os efeitos dos plugins: convites, canais app,
// invalidação de superfícies e mensagens criadas, editadas ou excluídas.
func PluginEffects() services.PluginEffects {
	return services.PluginEffects{
		Invite: func(ctx context.Context, v models.AppInvite) {
			event := websocket.NewNotificationOutbound{
				Type:              websocket.EventTypeNewNotification,
				ID:                v.ID,
				UserID:            v.InviterID,
				Kind:              "app_invite",
				AppInstallationID: v.InstallationID,
				Payload:           v,
				ExpiresAt:         &v.ExpiresAt,
			}
			websocket.GetHub().BroadcastToUsers(event, map[string]bool{v.UserID: true})
		},
		Channels:   BroadcastAppChannelChanges,
		Invalidate: BroadcastAppInvalidation,
		Created: func(ctx context.Context, m models.MessageWithAttachment, owner string) {
			publishCreatedMessageCtx(ctx, "plugin", m, owner)
		},
		Edited: func(ctx context.Context, m models.MessageWithAttachment, owner string) {
			event := websocket.MessageEditOutbound{
				Type:       websocket.EventTypeMessageEdit,
				ID:         m.ID,
				ChannelID:  m.ChannelID,
				Content:    derefString(m.Content),
				EditedAt:   derefTime(m.EditedAt),
				Components: &m.Components,
			}
			broadcastChannelEventCtx(ctx, "plugin", m.ChannelID, event)
			broadcastMessageEmbedsUpdate(ctx, "plugin", m.ChannelID, m.ID)
			go processEditedMessageEmbeds(context.Background(), "plugin", m.ChannelID, m.ID, owner, derefString(m.Content), m.Embeds)
		},
		Deleted: func(ctx context.Context, ch, id string) {
			event := websocket.MessageDeleteOutbound{Type: websocket.EventTypeMessageDelete, ID: id, ChannelID: ch}
			broadcastChannelEventCtx(ctx, "plugin", ch, event)
		},
	}
}

// remoteManagementOnly mantém grants/settings/escopos de plugins atrás do consentimento.
func remoteManagementOnly(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		a, err := storage.GetApp(c.Request().Context(), c.Param("app_id"))
		if err == nil && a.Kind == "plugin" {
			return appProblem(c, "", services.ErrPermissionDenied)
		}
		return next(c)
	}
}
