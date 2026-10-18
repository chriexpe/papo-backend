package middleware

import (
	"net/http"

	"github.com/labstack/echo/v4"
	echoMiddleware "github.com/labstack/echo/v4/middleware"
)

// CORS aplica os cabeçalhos CORS para os origins permitidos, habilitando
// requisições cross-origin tanto em HTTP quanto em HTTPS (ex.: frontend de
// desenvolvimento local). AllowCredentials é necessário porque a
// autenticação usa o cookie Auth (HttpOnly) em requisições cross-origin.
func CORS(origins []string) echo.MiddlewareFunc {
	cors := echoMiddleware.CORSWithConfig(echoMiddleware.CORSConfig{
		AllowOrigins:     origins,
		AllowMethods:     []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions},
		AllowHeaders:     []string{echo.HeaderContentType, echo.HeaderAuthorization, echo.HeaderXRequestID},
		ExposeHeaders:    []string{echo.HeaderXRequestID},
		AllowCredentials: true,
	})
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		handler := cors(next)
		return func(c echo.Context) error {
			if c.Request().Header.Get(echo.HeaderOrigin) == "null" {
				// Nunca refletir a origem opaca, nem com wildcard ou configuração explícita.
				if c.Request().Method == http.MethodOptions {
					return c.NoContent(http.StatusNoContent)
				}
				return next(c)
			}
			return handler(c)
		}
	}
}
