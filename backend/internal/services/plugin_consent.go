package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"net/url"
	"papo/internal/config"
	"papo/internal/models"
	"papo/internal/pluginbundle"
	"papo/internal/plugins"
	"papo/internal/storage"
	"papo/internal/utils"
)

// PluginOptions é a seleção administrativa usada tanto no preview quanto na aprovação.
type PluginOptions struct {
	StorageQuota *models.AppStorageQuotaOverride `json:"storage_quota,omitempty"`
	Settings     map[string]any                  `json:"settings"`
	Permissions  *[]string                       `json:"permissions,omitempty"`
	Scopes       []string                        `json:"scopes,omitempty"`
	MaxMemoryMB  int                             `json:"max_memory_mb,omitempty"`
	Consent      string                          `json:"consent,omitempty"`
	ExpiresAt    int64                           `json:"expires_at,omitempty"`
}

// PluginPreview inclui tudo que será concedido e a revisão que invalida aprovações antigas.
type PluginPreview struct {
	StorageQuota          models.AppStorageQuota         `json:"storage_quota"`
	StorageQuotaOverrides models.AppStorageQuotaOverride `json:"storage_quota_overrides"`
	ID                    string                         `json:"id"`
	Digest                string                         `json:"digest"`
	Version               string                         `json:"version"`
	Name                  string                         `json:"name"`
	Settings              map[string]any                 `json:"settings"`
	RequestedPermissions  []string                       `json:"requested_permissions"`
	Permissions           []string                       `json:"permissions"`
	BlockedPermissions    []string                       `json:"blocked_permissions"`
	Scopes                []string                       `json:"scopes"`
	ExplicitScopes        []string                       `json:"explicit_scopes"`
	Origins               []string                       `json:"origins"`
	Surfaces              []AppSurfaceDeclaration        `json:"surfaces"`
	MaxMemoryMB           int                            `json:"max_memory_mb"`
	Revision              int64                          `json:"revision"`
	State                 string                         `json:"state"`
	ExpiresAt             int64                          `json:"expires_at"`
	Consent               string                         `json:"consent"`
}

// Preview revalida pasta, settings e leitura humana dos escopos antes de assinar o consentimento.
func (m *PluginManager) Preview(ctx context.Context, actor, id string, o PluginOptions) (PluginPreview, error) {
	b, e := m.bundle(ctx, id, true)
	if errors.Is(e, plugins.ErrBusy) {
		return PluginPreview{}, e
	}
	if e != nil {
		return PluginPreview{}, fmt.Errorf("%w: %v", ErrInvalidInput, e)
	}
	p, e := m.preview(ctx, actor, b, o)
	if e == nil {
		p.Consent = m.consentToken(actor, p)
	}
	return p, e
}

// preview resolve valores e fontes sem conceder nada nem instanciar Wasm.
func (m *PluginManager) preview(ctx context.Context, actor string, b pluginbundle.Bundle, o PluginOptions) (PluginPreview, error) {
	p := PluginPreview{ID: b.Manifest.ID, Digest: b.Digest, Version: b.Manifest.Version, Name: b.Manifest.Name, Settings: map[string]any{}, RequestedPermissions: sorted(b.Manifest.Permissions.Request), Permissions: []string{}, BlockedPermissions: []string{}, Scopes: []string{}, ExplicitScopes: []string{}, Origins: []string{}, Surfaces: []AppSurfaceDeclaration{}, MaxMemoryMB: b.Manifest.Runtime.MaxMemoryMB, State: "discovered", ExpiresAt: o.ExpiresAt}
	if p.ExpiresAt == 0 {
		p.ExpiresAt = time.Now().Add(15 * time.Minute).Unix()
	}
	if p.ExpiresAt < time.Now().Unix() || p.ExpiresAt > time.Now().Add(15*time.Minute+time.Second).Unix() {
		return p, ErrPluginConsent
	}
	current, e := storage.FindPlugin(ctx, p.ID)
	if e != nil && e != storage.ErrNotFound {
		return p, e
	}
	if e == nil {
		p.StorageQuotaOverrides = current.StorageQuotaOverrides
		p.Revision = current.PluginRevision
		p.State = current.PluginState
		p.MaxMemoryMB = min(current.MaxMemoryMB, p.MaxMemoryMB)
		_ = json.Unmarshal(current.PluginSettings, &p.Settings)
		_ = json.Unmarshal(current.ExplicitScopes, &p.ExplicitScopes)
	}
	if o.StorageQuota != nil {
		v := o.StorageQuota
		if v.MaxKeys != nil {
			p.StorageQuotaOverrides.MaxKeys = v.MaxKeys
		}
		if v.MaxTotalBytes != nil {
			p.StorageQuotaOverrides.MaxTotalBytes = v.MaxTotalBytes
		}
		if v.MaxValueBytes != nil {
			p.StorageQuotaOverrides.MaxValueBytes = v.MaxValueBytes
		}
	}
	p.StorageQuota, e = config.EffectiveAppStorageQuota(p.StorageQuotaOverrides)
	if e != nil {
		return p, ErrInvalidInput
	}
	values := map[string]any{}
	for _, s := range b.Manifest.Settings {
		if s.Default != nil {
			values[s.Key] = s.Default
		}
		if v, ok := p.Settings[s.Key]; ok {
			values[s.Key] = v
		}
	}
	known := map[string]pluginbundle.Setting{}
	for _, s := range b.Manifest.Settings {
		known[s.Key] = s
	}
	for k, v := range o.Settings {
		if _, ok := known[k]; !ok {
			return p, ErrInvalidInput
		}
		values[k] = v
	}
	p.Settings = values
	for _, s := range b.Manifest.Settings {
		value, ok := p.Settings[s.Key]
		if !ok || value == nil {
			if s.Required {
				return p, fmt.Errorf("%w: setting %s obrigatório", ErrInvalidInput, s.Key)
			}
			delete(p.Settings, s.Key)
			continue
		}
		valid := false
		switch s.Type {
		case "string":
			v, ok := value.(string)
			valid = ok && len(v) <= 4096
		case "boolean":
			_, valid = value.(bool)
		case "integer":
			switch n := value.(type) {
			case int64:
				valid = true
			case int:
				valid = true
			case float64:
				valid = !math.IsNaN(n) && !math.IsInf(n, 0) && math.Trunc(n) == n && math.Abs(n) <= 9007199254740991
			}
		case "channel":
			v, ok := value.(string)
			_, err := uuid.Parse(v)
			valid = ok && err == nil
			if valid {
				canonical, _ := uuid.Parse(v)
				p.Settings[s.Key] = canonical.String()
				p.Scopes = append(p.Scopes, canonical.String())
			}
		case "https_url":
			v, ok := value.(string)
			if ok {
				v, err := validateRemoteSurfaceURL(ctx, v, config.LoadConfig().BaseURL)
				if err == nil {
					p.Settings[s.Key] = v
					u, _ := url.Parse(v)
					p.Origins = append(p.Origins, utils.OriginURL(u))
					valid = true
				}
			}
		}
		if !valid {
			return p, fmt.Errorf("%w: setting %s inválido", ErrInvalidInput, s.Key)
		}
	}
	if o.Scopes != nil {
		p.ExplicitScopes = o.Scopes
	}
	for i, v := range p.ExplicitScopes {
		id, e := uuid.Parse(v)
		if e != nil {
			return p, ErrInvalidInput
		}
		p.ExplicitScopes[i] = id.String()
	}
	p.ExplicitScopes = sorted(p.ExplicitScopes)
	p.Scopes = sorted(append(p.Scopes, p.ExplicitScopes...))
	for _, channel := range p.Scopes {
		if _, e = uuid.Parse(channel); e != nil {
			return p, ErrInvalidInput
		}
		ch, e := storage.GetChannelByID(ctx, channel)
		if e != nil {
			return p, e
		}
		if ch.Type != "text" && ch.Type != "voice" {
			return p, ErrPermissionDenied
		}
		if actor != "" {
			if e = requireWebhookChannelRead(ctx, ch, actor); e != nil {
				return p, e
			}
		}
	}
	for _, s := range b.Manifest.RemoteOrigins {
		normalized, e := validateRemoteSurfaceURL(ctx, s, config.LoadConfig().BaseURL)
		if e != nil {
			return p, e
		}
		u, _ := url.Parse(normalized)
		p.Origins = append(p.Origins, utils.OriginURL(u))
	}
	p.Origins = sorted(p.Origins)
	for _, s := range b.Manifest.Surfaces {
		d := AppSurfaceDeclaration{ID: s.ID, Kind: s.Kind, DefaultName: s.DefaultName, Source: "bundled", Entry: s.Entry, Size: s.Size, Load: s.Load, Display: s.Display, Poster: s.Poster}
		if d.Size.Width == 0 && d.Size.Height == 0 {
			d.Size = models.AppSurfaceSize{Width: s.Frame.Width, Height: s.Frame.Height}
		}
		if s.EntryURL != "" {
			d.Source = "remote"
			d.Entry = s.EntryURL
		}
		if s.EntryURLSetting != "" {
			v, ok := p.Settings[s.EntryURLSetting].(string)
			if !ok || v == "" {
				return p, ErrInvalidInput
			}
			d.Source = "remote"
			d.Entry = v
		}
		p.Surfaces = append(p.Surfaces, d)
	}
	p.Surfaces, e = validateAppSurfaces(ctx, PluginSurfaceBundle{Directory: b.Directory, ApprovedOrigins: p.Origins}, p.Surfaces)
	if e != nil {
		return p, e
	}
	for _, permission := range p.RequestedPermissions {
		if !m.Config.PermissionAllowed(permission) {
			p.BlockedPermissions = append(p.BlockedPermissions, permission)
		} else {
			p.Permissions = append(p.Permissions, permission)
		}
	}
	if o.Permissions != nil {
		p.Permissions = sorted(*o.Permissions)
		for _, permission := range p.Permissions {
			if !has(p.RequestedPermissions, permission) || !m.Config.PermissionAllowed(permission) {
				return p, ErrPermissionDenied
			}
		}
	}
	if has(p.Permissions, "post_app_embeds") && !has(p.Permissions, "send_messages") {
		return p, ErrInvalidInput
	}
	if o.MaxMemoryMB != 0 {
		p.MaxMemoryMB = o.MaxMemoryMB
	}
	if p.MaxMemoryMB < 1 || p.MaxMemoryMB > b.Manifest.Runtime.MaxMemoryMB || p.MaxMemoryMB > m.Config.MaxMemoryMB {
		return p, ErrInvalidInput
	}
	if e = m.Host.Validate(plugins.WithMemoryLimit(ctx, p.MaxMemoryMB), b.Module); e != nil {
		return p, e
	}
	return p, nil
}

// sorted normaliza conjuntos para comparação e assinatura determinísticas.
func sorted(values []string) []string {
	out := append([]string{}, values...)
	sort.Strings(out)
	j := 0
	for _, v := range out {
		if j == 0 || out[j-1] != v {
			out[j] = v
			j++
		}
	}
	return out[:j]
}

// has compara uma permissão/origem exata, sem curingas.
func has(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

// expanded detecta qualquer grant/origem fora do conjunto aprovado anteriormente.
func expanded(next, old []string) bool {
	for _, x := range next {
		if !has(old, x) {
			return true
		}
	}
	return false
}

// capabilities converte somente grants conhecidos para os checks comuns do REST.
func capabilities(p []string) models.AppCapabilities {
	return models.AppCapabilities{SendMessages: has(p, "send_messages"), ReadMessages: has(p, "read_messages"), PostAppEmbeds: has(p, "post_app_embeds"), PrivateStorage: has(p, "private_storage"), SendInvites: has(p, "send_invites")}
}

// jsonValue mantém uma representação SQL/JSON canônica sem expor objetos mutáveis.
func jsonValue(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
