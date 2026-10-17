package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/labstack/echo/v4"
	"io"
	"papo/internal/middleware"
	"papo/internal/services"
	"papo/internal/utils"
	"strings"
)

// appJSON recusa campos desconhecidos, corpos grandes e múltiplos documentos.
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
func interactionProblem(c echo.Context, base string, e error) error {
	switch {
	case errors.Is(e, context.DeadlineExceeded):
		return utils.SendProblem(c, base, 504, "app-timeout", "O app não respondeu a tempo", "Tente novamente mais tarde.")
	case errors.Is(e, services.ErrAppSession):
		return utils.SendProblem(c, base, 401, "app-session", "Sessão do app inválida", e.Error())
	case errors.Is(e, services.ErrAppRateLimit):
		c.Response().Header().Set("Retry-After", "60")
		return utils.SendProblem(c, base, 429, "rate-limit", "Limite de chamadas", e.Error())
	}
	return pluginProblem(c, base, e)
}

// appBridgeCORS autoriza apenas os dois endpoints públicos, sem ler cookies.
func appBridgeCORS(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		h := c.Response().Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Del("Access-Control-Allow-Credentials")
		h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		h.Set("Cache-Control", "no-store")
		if c.Request().Method == "OPTIONS" {
			return c.NoContent(204)
		}
		return next(c)
	}
}

// RegisterAppInteractionRoutes separa sessão humana de tokens confinados da UI.
func RegisterAppInteractionRoutes(e *echo.Echo, base string, m *services.PluginManager) {
	e.POST("/interactions", func(c echo.Context) error {
		var in services.InteractionInput
		if er := appJSON(c, &in); er != nil {
			return interactionProblem(c, base, er)
		}
		user, _ := c.Get(middleware.UserIDContextKey).(string)
		result, er := m.Interact(c.Request().Context(), user, in)
		if er != nil {
			return interactionProblem(c, base, er)
		}
		return c.JSON(200, result)
	}, middleware.JWTMiddleware)
	e.POST("/apps/:app_id/launch", func(c echo.Context) error {
		var in services.LaunchInput
		if er := appJSON(c, &in); er != nil {
			return interactionProblem(c, base, er)
		}
		user, _ := c.Get(middleware.UserIDContextKey).(string)
		v, er := m.LaunchAppUI(c.Request().Context(), user, c.Param("app_id"), in)
		if er != nil {
			return interactionProblem(c, base, er)
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(200, v)
	}, middleware.JWTMiddleware)
	for _, action := range []string{"session", "rpc"} {
		path := "/apps/:app_id/" + action
		e.OPTIONS(path, func(c echo.Context) error { return c.NoContent(204) }, appBridgeCORS)
		e.POST(path, func(c echo.Context) error {
			if action == "session" {
				var in struct {
					Ticket string `json:"ticket"`
				}
				if er := appJSON(c, &in); er != nil {
					return interactionProblem(c, base, er)
				}
				v, er := m.ExchangeAppTicket(c.Request().Context(), c.Param("app_id"), in.Ticket)
				if er != nil {
					return interactionProblem(c, base, er)
				}
				return c.JSON(200, v)
			}
			var in struct {
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if er := appJSON(c, &in); er != nil {
				return interactionProblem(c, base, er)
			}
			auth := c.Request().Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				return interactionProblem(c, base, services.ErrAppSession)
			}
			out, er := m.AppRPC(c.Request().Context(), c.Param("app_id"), strings.TrimPrefix(auth, "Bearer "), in.Method, in.Params)
			if er != nil {
				return interactionProblem(c, base, er)
			}
			return c.JSONBlob(200, out)
		}, appBridgeCORS)
	}
	for _, decision := range []string{"accept", "decline"} {
		e.POST("/notifications/:id/"+decision, func(c echo.Context) error {
			user, _ := c.Get(middleware.UserIDContextKey).(string)
			status := "declined"
			if decision == "accept" {
				status = "accepted"
			}
			v, er := m.DecideInvite(c.Request().Context(), user, c.Param("id"), status)
			if er != nil {
				return interactionProblem(c, base, er)
			}
			return c.JSON(200, v)
		}, middleware.JWTMiddleware)
	}
	e.PATCH("/apps/:app_id/user-settings", func(c echo.Context) error {
		var in struct {
			Muted *bool `json:"invites_muted"`
		}
		if er := appJSON(c, &in); er != nil || in.Muted == nil {
			return interactionProblem(c, base, services.ErrInvalidInput)
		}
		user, _ := c.Get(middleware.UserIDContextKey).(string)
		if er := services.SetPluginInviteMute(c.Request().Context(), user, c.Param("app_id"), *in.Muted); er != nil {
			return interactionProblem(c, base, er)
		}
		return c.NoContent(204)
	}, middleware.JWTMiddleware)
}
