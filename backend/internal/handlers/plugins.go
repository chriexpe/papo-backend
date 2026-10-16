package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

// pluginOptions limita seleção administrativa e rejeita campos desconhecidos.
func pluginOptions(c echo.Context) (services.PluginOptions, error) {
	var o services.PluginOptions
	d := json.NewDecoder(io.LimitReader(c.Request().Body, plugins.MaxPayload+1))
	d.DisallowUnknownFields()
	if e := d.Decode(&o); e != nil {
		return o, services.ErrInvalidInput
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return o, services.ErrInvalidInput
	}
	return o, nil
}

// PluginHandler implementa descoberta, consentimento e lifecycle atrás de manage_apps.
func PluginHandler(baseURL string, m *services.PluginManager, action string, c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
	defer cancel()
	actor, _ := c.Get(middleware.UserIDContextKey).(string)
	id := c.Param("id")
	switch action {
	case "list":
		list, e := m.List(ctx)
		if e != nil {
			return pluginProblem(c, baseURL, e)
		}
		return c.JSON(200, map[string]any{"plugins": list})
	case "rescan":
		if e := m.Rescan(ctx); e != nil {
			return pluginProblem(c, baseURL, e)
		}
		list, e := m.List(ctx)
		if e != nil {
			return pluginProblem(c, baseURL, e)
		}
		return c.JSON(200, map[string]any{"plugins": list})
	case "disable":
		if e := m.Disable(ctx, actor, id); e != nil {
			return pluginProblem(c, baseURL, e)
		}
		return c.NoContent(204)
	case "trigger":
		data, e := io.ReadAll(io.LimitReader(c.Request().Body, plugins.MaxPayload+1))
		if e != nil || len(data) > plugins.MaxPayload || !json.Valid(data) {
			return appProblem(c, baseURL, services.ErrInvalidInput)
		}
		out, e := m.Trigger(ctx, id, data)
		if e != nil {
			return pluginProblem(c, baseURL, e)
		}
		return c.JSONBlob(200, out)
	default:
		o, e := pluginOptions(c)
		if e != nil {
			return appProblem(c, baseURL, e)
		}
		switch action {
		case "preview":
			p, e := m.Preview(ctx, actor, id, o)
			if e != nil {
				return pluginProblem(c, baseURL, e)
			}
			return c.JSON(200, p)
		case "enable":
			a, e := m.Enable(ctx, actor, id, o)
			if e != nil {
				return pluginProblem(c, baseURL, e)
			}
			return c.JSON(200, a)
		case "patch":
			a, e := m.Patch(ctx, actor, id, o)
			if e != nil {
				return pluginProblem(c, baseURL, e)
			}
			return c.JSON(200, a)
		}
	}
	return c.NoContent(404)
}

// pluginProblem mantém erros de admissão/execução fora do envelope do guest.
func pluginProblem(c echo.Context, baseURL string, err error) error {
	switch {
	case errors.Is(err, services.ErrPluginConsent):
		return utils.SendProblem(c, baseURL, 409, "conflict", "Novo consentimento necessário", err.Error())
	case errors.Is(err, plugins.ErrInvalidArtifact):
		return appProblem(c, baseURL, services.ErrInvalidInput)
	case errors.Is(err, plugins.ErrBusy):
		return utils.SendProblem(c, baseURL, 429, "rate-limit", "Limite de plugins", "fila ou cache ocupado")
	case errors.Is(err, plugins.ErrQuarantined):
		return utils.SendProblem(c, baseURL, 409, "conflict", "Plugin em quarentena", err.Error())
	case errors.Is(err, plugins.ErrExecution), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return utils.SendProblem(c, baseURL, 422, "plugin-execution", "Falha no plugin", "execução interrompida ou resposta inválida")
	case errors.Is(err, storage.ErrNotFound):
		return appProblem(c, baseURL, services.ErrAppNotFound)
	default:
		return appProblem(c, baseURL, err)
	}
}

// PluginEffects usa os mesmos efeitos de mensagens e os snapshots de canais da PR 18.
func PluginEffects() services.PluginEffects {
	return services.PluginEffects{
		Channels: BroadcastAppChannelChanges,
		Created: func(ctx context.Context, m models.MessageWithAttachment, owner string) {
			publishCreatedMessageCtx(ctx, "plugin", m, owner)
		},
		Edited: func(ctx context.Context, m models.MessageWithAttachment, owner string) {
			broadcastChannelEventCtx(ctx, "plugin", m.ChannelID, websocket.MessageEditOutbound{Type: websocket.EventTypeMessageEdit, ID: m.ID, ChannelID: m.ChannelID, Content: derefString(m.Content), EditedAt: derefTime(m.EditedAt)})
			broadcastMessageEmbedsUpdate(ctx, "plugin", m.ChannelID, m.ID)
			go processEditedMessageEmbeds(context.Background(), "plugin", m.ChannelID, m.ID, owner, derefString(m.Content), m.Embeds)
		},
		Deleted: func(ctx context.Context, ch, id string) {
			broadcastChannelEventCtx(ctx, "plugin", ch, websocket.MessageDeleteOutbound{Type: websocket.EventTypeMessageDelete, ID: id, ChannelID: ch})
		},
	}
}

// remoteManagementOnly mantém grants/settings/escopos de plugins atrás do consentimento.
func remoteManagementOnly(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		a, e := storage.GetApp(c.Request().Context(), c.Param("app_id"))
		if e == nil && a.Kind == "plugin" {
			return appProblem(c, "", services.ErrPermissionDenied)
		}
		return next(c)
	}
}
