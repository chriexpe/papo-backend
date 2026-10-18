package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"papo/internal/middleware"
	"papo/internal/services"
	"papo/internal/utils"
)

// appJSON decodifica um corpo JSON de até 64 KiB, recusando campos
// desconhecidos e conteúdo depois do objeto.
func appJSON(c echo.Context, out any) error {
	d := json.NewDecoder(io.LimitReader(c.Request().Body, 65537))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return services.ErrInvalidInput
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return services.ErrInvalidInput
	}
	return nil
}

// interactionProblem mapeia erros das rotas humanas e da ponte UI.
func interactionProblem(c echo.Context, baseURL string, err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return utils.SendProblem(c, baseURL, http.StatusGatewayTimeout, "app-timeout",
			"O app não respondeu a tempo", "Tente novamente mais tarde.")
	case errors.Is(err, services.ErrAppSession):
		return utils.SendProblem(c, baseURL, http.StatusUnauthorized, "app-session", "Sessão do app inválida", err.Error())
	case errors.Is(err, services.ErrAppRateLimit):
		c.Response().Header().Set("Retry-After", "60")
		return utils.SendProblem(c, baseURL, http.StatusTooManyRequests, "rate-limit", "Limite de chamadas", err.Error())
	}
	return pluginProblem(c, baseURL, err)
}

// appBridgeCORS libera session e rpc para a origem opaca da UI, sem
// credenciais: essas rotas nunca aceitam cookies como autenticação.
func appBridgeCORS(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		h := c.Response().Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Del("Access-Control-Allow-Credentials")
		h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		h.Set("Cache-Control", "no-store")
		if c.Request().Method == http.MethodOptions {
			return c.NoContent(http.StatusNoContent)
		}
		return next(c)
	}
}

// InteractHandler implementa POST /interactions: executa on_interaction de um
// botão ou ação de usuário e devolve {interaction_id, result} a quem clicou.
func InteractHandler(baseURL string, m *services.PluginManager, c echo.Context) error {
	var in services.InteractionInput
	if err := appJSON(c, &in); err != nil {
		return interactionProblem(c, baseURL, err)
	}
	user, _ := c.Get(middleware.UserIDContextKey).(string)
	result, err := m.Interact(c.Request().Context(), user, in)
	if err != nil {
		return interactionProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusOK, result)
}

// LaunchAppUIHandler implementa POST /apps/:app_id/launch: emite um ticket de
// uso único para abrir a superfície.
func LaunchAppUIHandler(baseURL string, m *services.PluginManager, c echo.Context) error {
	var in services.LaunchInput
	if err := appJSON(c, &in); err != nil {
		return interactionProblem(c, baseURL, err)
	}
	user, _ := c.Get(middleware.UserIDContextKey).(string)
	v, err := m.LaunchAppUI(c.Request().Context(), user, c.Param("app_id"), in)
	if err != nil {
		return interactionProblem(c, baseURL, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, v)
}

// AppUISessionHandler implementa POST /apps/:app_id/session: troca o ticket
// por um token de sessão UI.
func AppUISessionHandler(baseURL string, m *services.PluginManager, c echo.Context) error {
	var in struct {
		Ticket string `json:"ticket"`
	}
	if err := appJSON(c, &in); err != nil {
		return interactionProblem(c, baseURL, err)
	}
	v, err := m.ExchangeAppTicket(c.Request().Context(), c.Param("app_id"), in.Ticket)
	if err != nil {
		return interactionProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusOK, v)
}

// AppUIRPCHandler implementa POST /apps/:app_id/rpc com o Bearer da sessão UI
// e devolve o envelope do on_ui_call.
func AppUIRPCHandler(baseURL string, m *services.PluginManager, c echo.Context) error {
	var in struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := appJSON(c, &in); err != nil {
		return interactionProblem(c, baseURL, err)
	}
	auth := c.Request().Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return interactionProblem(c, baseURL, services.ErrAppSession)
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	out, err := m.AppRPC(c.Request().Context(), c.Param("app_id"), token, in.Method, in.Params)
	if err != nil {
		return interactionProblem(c, baseURL, err)
	}
	return c.JSONBlob(http.StatusOK, out)
}

// DecideInviteHandler implementa POST /notifications/:id/accept e /decline,
// somente pelo destinatário do convite.
func DecideInviteHandler(baseURL string, m *services.PluginManager, status string, c echo.Context) error {
	user, _ := c.Get(middleware.UserIDContextKey).(string)
	v, err := m.DecideInvite(c.Request().Context(), user, c.Param("id"), status)
	if err != nil {
		return interactionProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusOK, v)
}

// AppUserSettingsHandler implementa PATCH /apps/:app_id/user-settings: a
// preferência pessoal invites_muted.
func AppUserSettingsHandler(baseURL string, c echo.Context) error {
	var in struct {
		Muted *bool `json:"invites_muted"`
	}
	if err := appJSON(c, &in); err != nil || in.Muted == nil {
		return interactionProblem(c, baseURL, services.ErrInvalidInput)
	}
	user, _ := c.Get(middleware.UserIDContextKey).(string)
	if err := services.SetPluginInviteMute(c.Request().Context(), user, c.Param("app_id"), *in.Muted); err != nil {
		return interactionProblem(c, baseURL, err)
	}
	return c.NoContent(http.StatusNoContent)
}
