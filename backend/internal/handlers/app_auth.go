package handlers

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"golang.org/x/time/rate"
	"papo/internal/services"
	"papo/internal/utils"
)

// appBucket mantém o limite independente por instalação autenticada.
type appBucket struct {
	limiter *rate.Limiter
	seen    time.Time
}

// appAuthentication autentica Bearer e aplica token bucket de 10/s, com burst 20.
// Entradas inativas expiram para limitar a memória sem um worker permanente.
func appAuthentication(baseURL string) echo.MiddlewareFunc {
	var mu sync.Mutex
	buckets := map[string]appBucket{}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			fields := strings.Fields(c.Request().Header.Get("Authorization"))
			if len(fields) != 2 || fields[0] != "Bearer" {
				return appProblem(c, baseURL, services.ErrAppUnauthorized)
			}
			a, err := services.AuthenticateApp(c.Request().Context(), fields[1])
			if err != nil {
				return appProblem(c, baseURL, err)
			}
			now := time.Now()
			mu.Lock()
			for id, b := range buckets {
				if now.Sub(b.seen) > 5*time.Minute {
					delete(buckets, id)
				}
			}
			b, ok := buckets[a.ID]
			if !ok {
				b.limiter = rate.NewLimiter(10, 20)
			}
			b.seen = now
			buckets[a.ID] = b
			allowed := b.limiter.Allow()
			mu.Unlock()
			if !allowed {
				c.Response().Header().Set("Retry-After", "1")
				return utils.SendProblem(c, baseURL, http.StatusTooManyRequests, "rate-limit", "Muitas requisições", "limite de requisições do app excedido")
			}
			c.Set("app_installation", a)
			return next(c)
		}
	}
}
