package services

import (
	"sync"
	"time"
)

// appLimitWindow é a janela fixa dos limites por minuto.
const appLimitWindow = time.Minute

// appLimiter aplica tetos por minuto em memória, com uma janela fixa por chave
// (usuário, sessão ou instalação). O backend roda em um único processo, como
// o middleware de rate limit e o limite dos apps remotos, então o contador não
// precisa passar pelo banco.
type appLimiter struct {
	mu      sync.Mutex
	windows map[string]*appLimitCounter
}

type appLimitCounter struct {
	start time.Time
	count int
}

func newAppLimiter() *appLimiter {
	return &appLimiter{windows: map[string]*appLimitCounter{}}
}

// allow conta uma chamada de key na janela atual e informa se ela cabe em
// perMinute. A janela recomeça um minuto depois da primeira chamada.
func (l *appLimiter) allow(key string, perMinute int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.windows[key]
	if w == nil || now.Sub(w.start) >= appLimitWindow {
		w = &appLimitCounter{start: now}
		l.windows[key] = w
	}
	w.count++
	return w.count <= perMinute
}

// sweep descarta janelas já encerradas.
func (l *appLimiter) sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, w := range l.windows {
		if now.Sub(w.start) >= appLimitWindow {
			delete(l.windows, key)
		}
	}
}

// limit retorna ErrAppRateLimit quando key excedeu perMinute na janela atual.
func (m *PluginManager) limit(key string, perMinute int) error {
	if !m.limits.allow(key, perMinute, time.Now()) {
		return ErrAppRateLimit
	}
	return nil
}
