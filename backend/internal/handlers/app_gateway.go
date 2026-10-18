package handlers

import (
	"context"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"papo/internal/appgateway"
	"papo/internal/models"
	"papo/internal/services"
)

// AppGatewayHandler promove somente Bearer autenticado e revalida o segredo durante a conexão.
// Não exige leitura global: tombstones capturados continuam válidos após perder read_messages.
func AppGatewayHandler(c echo.Context, registry *appgateway.Registry) error {
	app := c.Get("app_installation").(models.AppInstallation)
	token := strings.Fields(c.Request().Header.Get("Authorization"))[1]
	err := registry.Serve(c.Response(), c.Request(), app.ID, func(ctx context.Context) error {
		_, err := services.AuthenticateApp(ctx, token)
		return err
	})
	if err == nil {
		c.Response().Status = http.StatusSwitchingProtocols
	}
	return err
}
