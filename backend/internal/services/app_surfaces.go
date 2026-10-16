package services

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"net"
	"net/url"
	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/storage"
	"papo/internal/utils"
	"path"
	"strings"
	"sync"
	"unicode/utf8"
)

// AppChannelChange representa o estado anterior e posterior confirmado de um canal app.
type AppChannelChange = models.AppChannelChange

// AppSurfaceDeclaration vem do manifest validado pela PR de plugins.
type AppSurfaceDeclaration = models.AppSurfaceDeclaration

// ErrAppChannelManaged aponta para o lifecycle do app, nunca para exclusão manual.
var ErrAppChannelManaged = errors.New("este canal pertence a um app; desative o app nas configurações para removê-lo ou alterar seu tipo")

// PluginSurfaceBundle contém apenas metadados aprovados; não executa código do plugin.
type PluginSurfaceBundle struct {
	Directory       string
	Digest          string
	Enabled         bool
	ApprovedOrigins []string
	Surfaces        []AppSurfaceDeclaration
}

// PluginSurfaceSource é implementada pela descoberta/lifecycle de plugins na PR 5.
type PluginSurfaceSource interface {
	Resolve(context.Context, string) (PluginSurfaceBundle, error)
}

var pluginSurfaceSource struct {
	sync.RWMutex
	source PluginSurfaceSource
}

// SetPluginSurfaceSource configura a integração de metadados no início do servidor.
func SetPluginSurfaceSource(source PluginSurfaceSource) {
	pluginSurfaceSource.Lock()
	defer pluginSurfaceSource.Unlock()
	pluginSurfaceSource.source = source
}

// ResolvePluginSurfaceBundle falha fechado para instalações desativadas ou sem provedor.
func ResolvePluginSurfaceBundle(ctx context.Context, id string) (PluginSurfaceBundle, error) {
	a, err := activeApp(ctx, id)
	if err != nil {
		return PluginSurfaceBundle{}, err
	}
	if a.Kind == "remote" || a.Kind == "view" {
		return PluginSurfaceBundle{}, ErrPermissionDenied
	}
	pluginSurfaceSource.RLock()
	source := pluginSurfaceSource.source
	pluginSurfaceSource.RUnlock()
	if source == nil {
		return PluginSurfaceBundle{}, ErrAppNotFound
	}
	b, err := source.Resolve(ctx, id)
	if err != nil {
		return b, err
	}
	if !b.Enabled {
		return b, ErrAppNotFound
	}
	return b, nil
}

// validateRemoteSurfaceURL aplica as regras de embed, HTTPS, host público e origem distinta.
// BASE_URL vem da configuração, nunca do Host enviado na requisição.
func validateRemoteSurfaceURL(ctx context.Context, raw, baseURL string) (string, error) {
	if utf8.RuneCountInString(raw) > 2048 {
		return "", ErrInvalidInput
	}
	_, err := validateEmbedURL(raw)
	if err != nil {
		return "", ErrInvalidInput
	}
	original, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", ErrInvalidInput
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Hostname() == "" {
		return "", ErrInvalidInput
	}
	host := strings.TrimSuffix(strings.ToLower(original.Hostname()), ".")
	serverHost := strings.TrimSuffix(strings.ToLower(base.Hostname()), ".")
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	if ip := net.ParseIP(serverHost); ip != nil {
		serverHost = ip.String()
	}
	if original.Scheme != "https" || host == serverHost {
		return "", ErrInvalidInput
	}
	if err := utils.ValidatePublicHost(ctx, host); err != nil {
		return "", ErrInvalidInput
	}
	// NormalizeURL remove os colchetes IPv6; preserva a autoridade válida neste contrato.
	u := original
	u.Scheme = "https"
	u.Fragment = ""
	if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	return u.String(), nil
}

// safeBundledEntry aceita somente arquivos relativos confinados ao diretório ui/.
func safeBundledEntry(entry string) bool {
	return strings.HasPrefix(entry, "ui/") && !strings.ContainsAny(entry, "\\\x00?#%:") && path.Clean(entry) == entry && entry != "ui/" && !strings.HasSuffix(entry, "/")
}

// validateAppSurfaces normaliza entradas sem permitir origens fora do consentimento.
func validateAppSurfaces(ctx context.Context, b PluginSurfaceBundle, surfaces []AppSurfaceDeclaration) ([]AppSurfaceDeclaration, error) {
	out := append([]AppSurfaceDeclaration(nil), surfaces...)
	seen := make(map[string]bool)
	for i := range out {
		s := &out[i]
		if !validAppName(s.ID, 100) || strings.ContainsAny(s.ID, "/\\") || seen[s.ID] || (s.Kind != "channel" && s.Kind != "embed") {
			return nil, ErrInvalidInput
		}
		seen[s.ID] = true
		if s.Kind == "channel" && (!validAppName(s.DefaultName, maxChannelNameLength)) {
			return nil, ErrInvalidInput
		}
		switch s.Source {
		case "bundled":
			if !safeBundledEntry(s.Entry) {
				return nil, ErrInvalidInput
			}
		case "remote":
			normalized, err := validateRemoteSurfaceURL(ctx, s.Entry, config.LoadConfig().BaseURL)
			if err != nil {
				return nil, err
			}
			u, _ := url.Parse(normalized)
			origin := utils.OriginURL(u)
			approved := false
			for _, raw := range b.ApprovedOrigins {
				v, err := validateRemoteSurfaceURL(ctx, raw, config.LoadConfig().BaseURL)
				if err != nil {
					return nil, err
				}
				o, _ := url.Parse(v)
				if o.Path != "" && o.Path != "/" || o.RawQuery != "" {
					return nil, ErrInvalidInput
				}
				if origin == utils.OriginURL(o) {
					approved = true
				}
			}
			if !approved {
				return nil, ErrPermissionDenied
			}
			s.Entry = normalized
		default:
			return nil, ErrInvalidInput
		}
		if s.Kind == "embed" {
			if s.FrameWidth == 0 {
				s.FrameWidth = 480
			}
			if s.FrameHeight == 0 {
				s.FrameHeight = 320
			}
			if s.FrameWidth < 1 || s.FrameWidth > 1920 || s.FrameHeight < 1 || s.FrameHeight > 1080 {
				return nil, ErrInvalidInput
			}
		}
	}
	return out, nil
}

// CreateAppChannels cria canais declarados; repetir as mesmas superfícies é idempotente.
func CreateAppChannels(ctx context.Context, actorID, installationID string, surfaces []AppSurfaceDeclaration) ([]AppChannelChange, error) {
	return ReconcileAppChannels(ctx, actorID, installationID, surfaces)
}

// ReconcileAppChannels retorna mudanças após commit; o chamador publica os eventos.
func ReconcileAppChannels(ctx context.Context, actorID, installationID string, surfaces []AppSurfaceDeclaration) ([]AppChannelChange, error) {
	if _, err := uuid.Parse(actorID); actorID != "" && err != nil {
		return nil, ErrInvalidInput
	}
	b, err := ResolvePluginSurfaceBundle(ctx, installationID)
	if err != nil {
		return nil, err
	}
	validated, err := validateAppSurfaces(ctx, b, surfaces)
	if err != nil {
		return nil, err
	}
	changes, err := storage.ReconcileAppChannels(ctx, installationID, validated)
	if err != nil {
		return nil, err
	}
	RecordAudit(ctx, AuditEntry{ActorID: actorID, Action: ActionAppUpdate, EntityType: EntityApp, EntityID: &installationID, Metadata: map[string]any{"surfaces": len(validated)}})
	normalizeAppChanges(changes)
	return changes, nil
}

// DeleteAppChannels remove canais sem revogar a instalação; serve ao disable da PR 5.
func DeleteAppChannels(ctx context.Context, actorID, installationID string) ([]AppChannelChange, error) {
	if _, err := activeApp(ctx, installationID); err != nil {
		return nil, err
	}
	changes, err := storage.ReconcileAppChannels(ctx, installationID, nil)
	if err != nil {
		return nil, err
	}
	RecordAudit(ctx, AuditEntry{ActorID: actorID, Action: ActionAppUpdate, EntityType: EntityApp, EntityID: &installationID, Metadata: map[string]any{"surfaces_removed": true}})
	normalizeAppChanges(changes)
	return changes, nil
}

// CanTypeChannel permite typing apenas em canais que têm conversa.
func CanTypeChannel(ctx context.Context, channelID, userID string) error {
	ch, err := storage.GetChannelByID(ctx, channelID)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrChannelNotFound
	}
	if err != nil {
		return err
	}
	if ch.Type != "text" && ch.Type != "voice" && ch.Type != "dm" {
		return ErrPermissionDenied
	}
	return CanReadChannel(ctx, channelID, userID)
}

// PluginUICSP valida as origens aprovadas antes de compor o header de sandbox opaco.
func PluginUICSP(ctx context.Context, b PluginSurfaceBundle) (string, error) {
	origins := make([]string, 0, len(b.ApprovedOrigins))
	for _, raw := range b.ApprovedOrigins {
		normalized, err := validateRemoteSurfaceURL(ctx, raw, config.LoadConfig().BaseURL)
		if err != nil {
			return "", err
		}
		u, _ := url.Parse(normalized)
		if (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
			return "", ErrInvalidInput
		}
		origins = append(origins, utils.OriginURL(u))
	}
	policy := "sandbox allow-scripts allow-forms; default-src 'self'"
	if len(origins) > 0 {
		policy += " " + strings.Join(origins, " ")
	}
	return policy, nil
}

// channelAppPayload mantém entradas indisponíveis sem capacidade de lançamento.
func channelAppPayload(ctx context.Context, id string) (*models.AppChannel, error) {
	app, err := storage.GetAppChannel(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := ResolvePluginSurfaceBundle(ctx, app.AppInstallationID); err != nil {
		app.Available = false
		app.Entry = ""
		return app, nil
	}
	if app.Source == "bundled" {
		app.Entry = "/apps/" + app.AppInstallationID + "/" + app.Entry
	}
	return app, nil
}

// normalizeAppChanges converte arquivos ui/ em rotas públicas depois do commit.
func normalizeAppChanges(changes []AppChannelChange) {
	for i := range changes {
		if after := changes[i].After; after != nil && after.App != nil && after.App.Source == "bundled" {
			after.App.Entry = "/apps/" + after.App.AppInstallationID + "/" + after.App.Entry
		}
	}
}

// AppChannelState monta metadados para broadcast, sem herdar a leitura do ator da gestão.
func AppChannelState(ctx context.Context, id string) (models.Channel, error) {
	ch, err := storage.GetChannelByID(ctx, id)
	if err != nil {
		return ch, err
	}
	if ch.Type == "app" {
		ch.App, err = channelAppPayload(ctx, id)
	}
	return ch, err
}
