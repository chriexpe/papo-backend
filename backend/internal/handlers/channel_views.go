package handlers

import (
	"github.com/labstack/echo/v4"
	"net/http"
	"papo/internal/middleware"
	"papo/internal/services"
)

// channelViewRequest define URL/posição e a instalação exigida na criação.
type channelViewRequest struct {
	AppInstallationID string `json:"app_installation_id"`
	URL               string `json:"url"`
	Position          int    `json:"position"`
}

// ListChannelViewsHandler lista views para gestores que também leem o canal.
func ListChannelViewsHandler(baseURL string, c echo.Context) error {
	actor, _ := c.Get(middleware.UserIDContextKey).(string)
	views, err := services.ListChannelViews(c.Request().Context(), actor, c.Param("channel_id"))
	if err != nil {
		return appProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusOK, views)
}

// SaveChannelViewHandler cria ou substitui uma view passiva sem receber credenciais.
func SaveChannelViewHandler(baseURL string, c echo.Context) error {
	actor, _ := c.Get(middleware.UserIDContextKey).(string)
	var req channelViewRequest
	if err := c.Bind(&req); err != nil {
		return appProblem(c, baseURL, services.ErrInvalidInput)
	}
	view, err := services.SaveChannelView(c.Request().Context(), actor, c.Param("channel_id"), c.Param("view_id"), req.AppInstallationID, req.URL, baseURL, req.Position)
	if err != nil {
		return appProblem(c, baseURL, err)
	}
	status := http.StatusOK
	if c.Param("view_id") == "" {
		status = http.StatusCreated
	}
	return c.JSON(status, view)
}

// DeleteChannelViewHandler destaca uma view do canal legível pelo gestor.
func DeleteChannelViewHandler(baseURL string, c echo.Context) error {
	actor, _ := c.Get(middleware.UserIDContextKey).(string)
	if err := services.DeleteChannelView(c.Request().Context(), actor, c.Param("channel_id"), c.Param("view_id")); err != nil {
		return appProblem(c, baseURL, err)
	}
	return c.NoContent(http.StatusNoContent)
}
