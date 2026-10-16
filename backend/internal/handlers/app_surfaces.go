package handlers

import (
	"context"
	"github.com/labstack/echo/v4"
	"net/http"
	"papo/internal/models"
	"papo/internal/services"
	"papo/internal/utils"
	"papo/internal/websocket"
)

// BroadcastAppChannelChanges publica snapshots confirmados aos leitores, inclusive após exclusão.
// Os hooks não publicam: enable/disable e workers da PR 5 reutilizam este contrato.
func BroadcastAppChannelChanges(ctx context.Context, changes []services.AppChannelChange) {
	hub := websocket.GetHub()
	for _, change := range changes {
		channel := change.After
		if channel == nil {
			channel = change.Before
		}
		if channel == nil {
			continue
		}
		var allowed map[string]bool
		var err error
		if change.After == nil {
			allowed, err = services.ChannelReadersSnapshot(ctx, *channel, hub.OnlineUserIDs())
		} else {
			allowed, err = services.ChannelReaders(ctx, channel.ID, hub.OnlineUserIDs())
		}
		if err != nil {
			utils.Errorf("websocket: falha ao autorizar canal app %s: %v", channel.ID, err)
			continue
		}
		if change.After == nil {
			hub.BroadcastToUsers(websocket.ChannelDeleteOutbound{Type: websocket.EventTypeChannelDelete, ChannelID: channel.ID}, allowed)
		} else if change.Before == nil {
			hub.BroadcastToUsers(websocket.ChannelCreateOutbound{Type: websocket.EventTypeChannelCreate, ChannelID: channel.ID, Name: channel.Name, Position: channel.Position, ChannelType: channel.Type, Topic: channel.Topic, App: channel.App}, allowed)
		} else {
			hub.BroadcastToUsers(websocket.ChannelUpdateOutbound{Type: websocket.EventTypeChannelUpdate, ChannelID: channel.ID, Name: channel.Name, Position: channel.Position, Topic: channel.Topic, App: channel.App}, allowed)
		}
	}
}

// broadcastChannelMetadata preserva o broadcast comum, mas restringe dados de app a leitores.
func broadcastChannelMetadata(c echo.Context, channel models.ChannelSummary, event websocket.ChannelUpdateOutbound) {
	if channel.Type == "app" {
		ch, err := services.AppChannelState(c.Request().Context(), channel.ID)
		if err != nil {
			utils.Errorf("websocket: falha ao consultar canal app: %v", err)
			return
		}
		BroadcastAppChannelChanges(c.Request().Context(), []services.AppChannelChange{{Before: &ch, After: &ch}})
	} else {
		websocket.GetHub().Broadcast(event)
	}
}

// appChannelProblem explica em português que canais app pertencem ao lifecycle.
func appChannelProblem(baseURL string, c echo.Context, err error) error {
	return utils.SendProblem(c, baseURL, http.StatusBadRequest, "invalid-param", "Parâmetro inválido", err.Error())
}

// broadcastAppChannelMetadata atualiza somente leitores quando as permissões de um canal app mudam.
func broadcastAppChannelMetadata(c echo.Context, id string) {
	ch, err := services.AppChannelState(c.Request().Context(), id)
	if err != nil {
		utils.Errorf("websocket: falha ao consultar canal %s: %v", id, err)
		return
	}
	if ch.Type == "app" {
		BroadcastAppChannelChanges(c.Request().Context(), []services.AppChannelChange{{Before: &ch, After: &ch}})
	}
}
