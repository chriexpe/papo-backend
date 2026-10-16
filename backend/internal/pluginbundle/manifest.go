package pluginbundle

import (
	"bytes"
	"fmt"
	"github.com/google/uuid"
	"net/url"
	"papo/internal/models"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)
var semver = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// Manifest é o contrato TOML v1; campos desconhecidos são erros.
type Manifest struct {
	ManifestVersion int      `toml:"manifest_version" json:"manifest_version"`
	ID              string   `toml:"id" json:"id"`
	Name            string   `toml:"name" json:"name"`
	Version         string   `toml:"version" json:"version"`
	MinPapoAPI      int      `toml:"min_papo_api" json:"min_papo_api"`
	RemoteOrigins   []string `toml:"remote_origins" json:"remote_origins"`
	Permissions     struct {
		Request []string `toml:"request" json:"request"`
	} `toml:"permissions" json:"permissions"`
	Runtime struct {
		MaxMemoryMB int `toml:"max_memory_mb" json:"max_memory_mb"`
	} `toml:"runtime" json:"runtime"`
	UserActions []models.AppUserAction `toml:"user_actions" json:"user_actions"`
	Settings    []Setting              `toml:"settings" json:"settings"`
	Surfaces    []Surface              `toml:"surfaces" json:"surfaces"`
}

// Setting declara um valor visível no consentimento; channel implica escopo explícito.
type Setting struct {
	Key      string `toml:"key" json:"key"`
	Type     string `toml:"type" json:"type"`
	Required bool   `toml:"required" json:"required"`
	Default  any    `toml:"default" json:"default,omitempty"`
}

// Surface aponta para um asset UI ou uma URL que será resolvida antes do consentimento.
type Surface struct {
	ID              string                `toml:"id" json:"id"`
	Kind            string                `toml:"kind" json:"kind"`
	DefaultName     string                `toml:"default_name" json:"default_name,omitempty"`
	Entry           string                `toml:"entry" json:"entry,omitempty"`
	EntryURL        string                `toml:"entry_url" json:"entry_url,omitempty"`
	EntryURLSetting string                `toml:"entry_url_setting" json:"entry_url_setting,omitempty"`
	Size            models.AppSurfaceSize `toml:"size" json:"size"`
	Load            string                `toml:"load" json:"load"`
	Display         string                `toml:"display" json:"display"`
	Poster          string                `toml:"poster" json:"poster"`
	Frame           struct {
		Width  int `toml:"width" json:"width"`
		Height int `toml:"height" json:"height"`
	} `toml:"frame" json:"frame"`
}

// SafePath aceita somente caminhos portáveis relativos e canônicos.
func SafePath(s string) bool {
	if s == "" || len(s) > 512 || !utf8.ValidString(s) || path.Clean(s) != s || strings.HasPrefix(s, "/") || s == ".." || strings.HasPrefix(s, "../") || strings.ContainsAny(s, "\\\x00%:?#") {
		return false
	}
	for _, p := range strings.Split(s, "/") {
		if strings.HasPrefix(p, ".") {
			return false
		}
	}
	return true
}

// Parse valida o schema sem executar o módulo ou fazer requisições de rede.
func Parse(data []byte, c Config) (Manifest, error) {
	var m Manifest
	d := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields()
	if e := d.Decode(&m); e != nil {
		return m, e
	}
	if m.ManifestVersion != 1 || !identifier.MatchString(m.ID) || !displayName(m.Name) || utf8.RuneCountInString(m.Name) > 80 || !validVersion(m.Version) || m.MinPapoAPI != 1 {
		return m, fmt.Errorf("identidade/API do manifest inválida")
	}
	if m.Runtime.MaxMemoryMB == 0 {
		m.Runtime.MaxMemoryMB = c.DefaultMemoryMB
	}
	if m.Runtime.MaxMemoryMB < 1 || m.Runtime.MaxMemoryMB > c.MaxMemoryMB {
		return m, fmt.Errorf("max_memory_mb inválido")
	}
	seen := map[string]bool{}
	for _, p := range m.Permissions.Request {
		if !knownPermission(p) || seen[p] {
			return m, fmt.Errorf("permissão inválida: %s", p)
		}
		seen[p] = true
	}
	if seen["post_app_embeds"] && !seen["send_messages"] {
		return m, fmt.Errorf("post_app_embeds exige send_messages")
	}
	settings := map[string]Setting{}
	for _, s := range m.Settings {
		if !identifier.MatchString(s.Key) {
			return m, fmt.Errorf("setting inválido")
		}
		if _, ok := settings[s.Key]; ok {
			return m, fmt.Errorf("setting duplicado")
		}
		switch s.Type {
		case "string", "boolean", "integer", "https_url", "channel":
		default:
			return m, fmt.Errorf("tipo de setting inválido")
		}
		if s.Default != nil && !validDefault(s) {
			return m, fmt.Errorf("default de setting inválido: %s", s.Key)
		}
		settings[s.Key] = s
	}
	seen = map[string]bool{}
	for _, s := range m.Surfaces {
		if !identifier.MatchString(s.ID) || seen[s.ID] || (s.Kind != "channel" && s.Kind != "embed") {
			return m, fmt.Errorf("superfície inválida")
		}
		seen[s.ID] = true
		n := 0
		for _, v := range []string{s.Entry, s.EntryURL, s.EntryURLSetting} {
			if v != "" {
				n++
			}
		}
		if n != 1 {
			return m, fmt.Errorf("superfície exige uma fonte")
		}
		if s.Entry != "" && (!SafePath(s.Entry) || !strings.HasPrefix(s.Entry, "ui/")) {
			return m, fmt.Errorf("entry inválido")
		}
		if s.EntryURL != "" {
			if _, e := HTTPSOrigin(s.EntryURL); e != nil {
				return m, e
			}
		}
		if s.EntryURLSetting != "" && settings[s.EntryURLSetting].Type != "https_url" {
			return m, fmt.Errorf("setting URL inválido")
		}
		if s.Kind == "channel" && (!displayName(s.DefaultName) || utf8.RuneCountInString(s.DefaultName) > 100) {
			return m, fmt.Errorf("nome do canal inválido")
		}
		if s.Load != "" && s.Load != "click" && s.Load != "visible" {
			return m, fmt.Errorf("load inválido")
		}
		if s.Display != "" && s.Display != "inline" && s.Display != "overlay" {
			return m, fmt.Errorf("display inválido")
		}
		if s.Display == "overlay" && s.Load == "visible" {
			return m, fmt.Errorf("overlay exige click")
		}
		if s.Size.Width < 0 || s.Size.Height < 0 || s.Size.Width > 1920 || s.Size.Height > 1080 {
			return m, fmt.Errorf("size inválido")
		}
		if s.Poster != "" && (!SafePath(s.Poster) || !strings.HasPrefix(s.Poster, "ui/")) {
			return m, fmt.Errorf("poster inválido")
		}
		if s.Frame.Width < 0 || s.Frame.Width > 1920 || s.Frame.Height < 0 || s.Frame.Height > 1080 {
			return m, fmt.Errorf("frame inválido")
		}
	}
	seen = map[string]bool{}
	if len(m.UserActions) > 20 {
		return m, fmt.Errorf("ações demais")
	}
	for _, a := range m.UserActions {
		if !identifier.MatchString(a.ID) || seen[a.ID] || !displayName(a.Label) || utf8.RuneCountInString(a.Label) > 80 {
			return m, fmt.Errorf("ação inválida")
		}
		seen[a.ID] = true
		if a.Icon != "" && (!SafePath(a.Icon) || !strings.HasPrefix(a.Icon, "ui/") || !imageExtension(a.Icon)) {
			return m, fmt.Errorf("ícone inválido")
		}
	}
	for _, s := range m.RemoteOrigins {
		o, e := HTTPSOrigin(s)
		if e != nil || o != s {
			return m, fmt.Errorf("origem inválida: %s", s)
		}
	}
	return m, nil
}

// HTTPSOrigin faz validação sintática; o serviço aplica a política SSRF/DNS da PR 18.
func HTTPSOrigin(raw string) (string, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Port() != "" && u.Port() != "443") || len(raw) > 2048 {
		return "", fmt.Errorf("URL HTTPS inválida")
	}
	return "https://" + strings.ToLower(u.Host), nil
}

// UIExtension coincide com o allowlist da rota pública da PR 18.
func UIExtension(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".html", ".css", ".js", ".mjs", ".json", ".png", ".jpg", ".jpeg", ".webp", ".gif", ".svg", ".ico", ".woff", ".woff2", ".ttf", ".otf", ".txt", ".wasm":
		return true
	}
	return false
}

// displayName recusa controles que não pertencem a nomes visíveis na UI.
func displayName(s string) bool {
	if strings.TrimSpace(s) == "" || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// validVersion completa a validação SemVer de identificadores e zeros iniciais.
func validVersion(s string) bool {
	if len(s) > 100 || !semver.MatchString(s) {
		return false
	}
	main := strings.SplitN(s, "+", 2)
	if len(main) == 2 {
		for _, v := range strings.Split(main[1], ".") {
			if v == "" {
				return false
			}
		}
	}
	pre := strings.SplitN(main[0], "-", 2)
	if len(pre) == 2 {
		for _, v := range strings.Split(pre[1], ".") {
			if v == "" {
				return false
			}
			numeric := true
			for _, r := range v {
				if r < '0' || r > '9' {
					numeric = false
				}
			}
			if numeric && len(v) > 1 && v[0] == '0' {
				return false
			}
		}
	}
	return true
}

// validDefault valida defaults locais sem rede nem acesso ao banco.
func validDefault(s Setting) bool {
	switch s.Type {
	case "string":
		v, ok := s.Default.(string)
		return ok && len(v) <= 4096
	case "boolean":
		_, ok := s.Default.(bool)
		return ok
	case "integer":
		_, ok := s.Default.(int64)
		return ok
	case "channel":
		v, ok := s.Default.(string)
		if !ok {
			return false
		}
		_, e := uuid.Parse(v)
		return e == nil
	case "https_url":
		v, ok := s.Default.(string)
		if !ok {
			return false
		}
		_, e := HTTPSOrigin(v)
		return e == nil
	}
	return false
}

// imageExtension mantém ícones de ações limitados a imagens estáticas da UI.
func imageExtension(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".svg", ".ico":
		return true
	}
	return false
}
