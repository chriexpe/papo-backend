package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"papo/internal/middleware"
	"papo/internal/models"
	"papo/internal/plugins"
	"papo/internal/services"
	"papo/internal/utils"

	"github.com/labstack/echo/v4"
)

// UploadPluginHandler recebe bytes Wasm limitados antes de validar ou compilar.
func UploadPluginHandler(baseURL string, host plugins.PluginHost, c echo.Context) error {
	data, err := io.ReadAll(io.LimitReader(c.Request().Body, plugins.MaxArtifact+1))
	if len(data) > plugins.MaxArtifact || errors.Is(err, middleware.ErrBodyLimitExceeded) {
		return utils.SendProblem(c, baseURL, 413, "payload-too-large", "Payload muito grande", "artefato excede 2 MiB")
	}
	if err != nil {
		return appProblem(c, baseURL, services.ErrInvalidInput)
	}
	actor, _ := c.Get(middleware.UserIDContextKey).(string)
	digest, err := services.UploadPlugin(c.Request().Context(), host, actor, c.Param("app_id"), data)
	if err != nil {
		return pluginProblem(c, baseURL, err)
	}
	return c.JSON(http.StatusOK, map[string]string{"artifact_digest": digest})
}

// TriggerPluginHandler executa JSON administrativo e reutiliza publicação de mensagens.
func TriggerPluginHandler(baseURL string, host plugins.PluginHost, c echo.Context) error {
	data, err := io.ReadAll(io.LimitReader(c.Request().Body, plugins.MaxPayload+1))
	if err != nil || len(data) > plugins.MaxPayload || !json.Valid(data) {
		return appProblem(c, baseURL, services.ErrInvalidInput)
	}
	out, err := services.InvokePlugin(c.Request().Context(), host, c.Param("app_id"), data, func(m models.MessageWithAttachment, owner string) { publishCreatedMessage(c, m, owner) })
	if err != nil {
		return pluginProblem(c, baseURL, err)
	}
	return c.JSONBlob(http.StatusOK, out)
}

// pluginProblem mantém erros de admissão e execução fora do envelope não confiável do guest.
func pluginProblem(c echo.Context, baseURL string, err error) error {
	switch {
	case errors.Is(err, plugins.ErrInvalidArtifact):
		return appProblem(c, baseURL, services.ErrInvalidInput)
	case errors.Is(err, plugins.ErrBusy):
		return utils.SendProblem(c, baseURL, 429, "rate-limit", "Limite de plugins", "fila ou cache ocupado")
	case errors.Is(err, plugins.ErrQuarantined):
		return utils.SendProblem(c, baseURL, 409, "conflict", "Plugin em quarentena", err.Error())
	case errors.Is(err, plugins.ErrExecution), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return utils.SendProblem(c, baseURL, 422, "plugin-execution", "Falha no plugin", "execução interrompida ou resposta inválida")
	default:
		return appProblem(c, baseURL, err)
	}
}
