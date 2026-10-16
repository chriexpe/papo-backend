package services

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"papo/internal/models"
	"papo/internal/pluginbundle"
	"papo/internal/plugins"
	"papo/internal/storage"
	"papo/internal/utils"
)

// ErrPluginConsent exige preview novo sem conceder permissões implicitamente.
var ErrPluginConsent = errors.New("consentimento desatualizado; gere e aprove um novo preview")

// PluginEffects mantém transportes na camada de handlers, inclusive no hot reload.
type PluginEffects struct {
	Invalidate func(context.Context, string)
	Channels   func(context.Context, []AppChannelChange)
	Created    func(context.Context, models.MessageWithAttachment, string)
	Edited     func(context.Context, models.MessageWithAttachment, string)
	Deleted    func(context.Context, string, string)
}

// DiscoveredPlugin apresenta erros sem executar ou habilitar candidatos inválidos.
type DiscoveredPlugin struct {
	ID           string                  `json:"id"`
	Manifest     *pluginbundle.Manifest  `json:"manifest,omitempty"`
	Digest       string                  `json:"digest,omitempty"`
	Error        string                  `json:"error,omitempty"`
	Installation *models.AppInstallation `json:"installation,omitempty"`
}
type pluginTransitionKey struct{}

type pluginGate struct {
	gate    chan struct{}
	pending int
}

// PluginManager liga descoberta, consentimento, runtime e hooks da PR 18.
// O catálogo guarda metadados; não retém cópias dos assets nem estado de guest.
type PluginManager struct {
	Config  pluginbundle.Config
	Host    plugins.PluginHost
	Effects PluginEffects
	secret  [32]byte
	mu      sync.Mutex
	catalog map[string]DiscoveredPlugin
	gates   map[string]*pluginGate
	scan    chan struct{}
	ioSlots chan struct{}
}

// NewPluginManager configura descoberta sem instanciar qualquer módulo.
func NewPluginManager(c pluginbundle.Config, h plugins.PluginHost, e PluginEffects) (*PluginManager, error) {
	m := &PluginManager{Config: c, Host: h, Effects: e, catalog: map[string]DiscoveredPlugin{}, gates: map[string]*pluginGate{}, scan: make(chan struct{}, 1), ioSlots: make(chan struct{}, 4)}
	_, err := rand.Read(m.secret[:])
	return m, err
}

// validate compartilha ABI e teto de memória com o CLI e com o runtime real.
func (m *PluginManager) validate(ctx context.Context, b []byte, mb int) error {
	return m.Host.Validate(plugins.WithMemoryLimit(ctx, mb), b)
}

// lock limita espera por plugin e mantém mudanças de versão na fronteira de invocação.
func (m *PluginManager) lock(ctx context.Context, id string) (func(), error) {
	m.mu.Lock()
	g := m.gates[id]
	if g == nil {
		if len(m.gates) >= 1024 {
			m.mu.Unlock()
			return nil, plugins.ErrBusy
		}
		g = &pluginGate{gate: make(chan struct{}, 1)}
		m.gates[id] = g
	}
	if g.pending >= 9 {
		m.mu.Unlock()
		return nil, plugins.ErrBusy
	}
	g.pending++
	m.mu.Unlock()
	release := func() {
		m.mu.Lock()
		g.pending--
		if g.pending == 0 {
			delete(m.gates, id)
		}
		m.mu.Unlock()
	}
	select {
	case g.gate <- struct{}{}:
		return func() { <-g.gate; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

// List associa o catálogo validado ao estado persistente sem revelar a pasta do host.
func (m *PluginManager) List(ctx context.Context) ([]DiscoveredPlugin, error) {
	installs, e := storage.ListPluginInstallations(ctx)
	if e != nil {
		return nil, e
	}
	byKey := map[string]models.AppInstallation{}
	for _, a := range installs {
		byKey[a.AppKey] = a
	}
	m.mu.Lock()
	out := make([]DiscoveredPlugin, 0, len(m.catalog))
	for _, d := range m.catalog {
		if a, ok := byKey[d.ID]; ok {
			v := a
			d.Installation = &v
			delete(byKey, d.ID)
		}
		out = append(out, d)
	}
	m.mu.Unlock()
	for _, a := range byKey {
		v := a
		out = append(out, DiscoveredPlugin{ID: a.AppKey, Error: "pasta ausente", Installation: &v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Rescan descobre pastas/pacotes sem instanciar; remoções desativam plugins conhecidos.
func (m *PluginManager) Rescan(ctx context.Context) error {
	select {
	case m.scan <- struct{}{}:
		defer func() { <-m.scan }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if e := os.MkdirAll(m.Config.Directory, 0750); e != nil {
		return e
	}
	rootInfo, e := os.Lstat(m.Config.Directory)
	if e != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return ErrInvalidInput
	}
	entries, e := os.ReadDir(m.Config.Directory)
	if e != nil {
		return e
	}
	if len(entries) > m.Config.Files {
		return fmt.Errorf("limite de candidatos excedido")
	}
	next := map[string]DiscoveredPlugin{}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(m.Config.Directory, name)
		if strings.HasSuffix(name, ".papoplugin") {
			if !entry.Type().IsRegular() {
				next[name] = DiscoveredPlugin{ID: name, Error: "pacote não regular"}
				continue
			}
			archiveInfo, e := entry.Info()
			if e != nil {
				return e
			}
			if archiveInfo.Size() > m.Config.FolderBytes+(1<<20) {
				next[name] = DiscoveredPlugin{ID: name, Error: "ZIP excede limite"}
				continue
			}
			// Pacotes já extraídos não substituem a pasta canônica nem a invalidam.
			if _, e = os.Lstat(filepath.Join(m.Config.Directory, strings.TrimSuffix(name, ".papoplugin"))); e == nil {
				continue
			}
			b, e := pluginbundle.Extract(ctx, full, m.Config.Directory, m.Config, m.validate)
			if e != nil {
				next[name] = DiscoveredPlugin{ID: name, Error: e.Error()}
				continue
			}
			v := b.Manifest
			next[v.ID] = DiscoveredPlugin{ID: v.ID, Manifest: &v, Digest: b.Digest}
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			next[name] = DiscoveredPlugin{ID: name, Error: "symlink recusado"}
			continue
		}
		if !entry.IsDir() {
			continue
		}
		if _, e := os.Lstat(filepath.Join(full, "plugin.wasm")); os.IsNotExist(e) {
			continue
		}
		b, e := pluginbundle.Load(ctx, full, m.Config, m.validate)
		if e != nil {
			next[name] = DiscoveredPlugin{ID: name, Error: e.Error()}
			continue
		}
		v := b.Manifest
		next[name] = DiscoveredPlugin{ID: name, Manifest: &v, Digest: b.Digest}
	}
	m.mu.Lock()
	m.catalog = next
	m.mu.Unlock()
	installs, e := storage.ListPluginInstallations(ctx)
	if e != nil {
		return e
	}
	for _, a := range installs {
		if a.PluginState != "enabled" && a.PluginState != "quarantined" {
			continue
		}
		opCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		unlock, e := m.lock(opCtx, a.AppKey)
		if e != nil {
			cancel()
			return e
		}
		d, exists := next[a.AppKey]
		if a.LifecyclePending {
			e = m.quarantine(opCtx, a)
		} else if !exists {
			e = m.disableLocked(opCtx, a, "", true)
		} else if d.Error != "" {
			e = m.quarantine(opCtx, a)
		} else if a.PluginDigest == nil || d.Digest != *a.PluginDigest {
			if m.Config.HotReload && a.PluginState == "enabled" {
				e = m.reloadLocked(opCtx, a)
			} else {
				e = m.quarantine(opCtx, a)
			}
		}
		unlock()
		cancel()
		if e != nil && !errors.Is(e, ErrPluginConsent) && !errors.Is(e, plugins.ErrQuarantined) {
			return e
		}
	}
	return nil
}

// Watch só faz polling quando solicitado; shutdown cancela espera e SQL.
func (m *PluginManager) Watch(ctx context.Context) {
	if !m.Config.HotReload {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if e := m.Rescan(ctx); e != nil && ctx.Err() == nil {
				utils.Errorf("plugins: rescan: %v", e)
			}
		}
	}
}

// bundle revalida todos os bytes, sem executar ou confiar no catálogo anterior.
func (m *PluginManager) bundle(ctx context.Context, id string, validate bool) (pluginbundle.Bundle, error) {
	select {
	case m.ioSlots <- struct{}{}:
		defer func() { <-m.ioSlots }()
	default:
		return pluginbundle.Bundle{}, plugins.ErrBusy
	}
	if !pluginID(id) {
		return pluginbundle.Bundle{}, ErrInvalidInput
	}
	var v pluginbundle.Validator
	if validate {
		v = m.validate
	}
	return pluginbundle.Load(ctx, filepath.Join(m.Config.Directory, id), m.Config, v)
}

// pluginID fecha os caminhos aceitos também antes da descoberta.
func pluginID(s string) bool {
	if len(s) < 1 || len(s) > 100 {
		return false
	}
	for i, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || i > 0 && (r == '.' || r == '_' || r == '-') {
			continue
		}
		return false
	}
	return true
}

// Resolve implementa a interface da PR 18 e falha fechado quando os bytes mudam.
func (m *PluginManager) Resolve(ctx context.Context, id string) (PluginSurfaceBundle, error) {
	a, e := storage.GetApp(ctx, id)
	if e != nil {
		return PluginSurfaceBundle{}, e
	}
	if a.Kind != "plugin" || a.RevokedAt != nil || a.PluginState != "enabled" || a.PluginDigest == nil || (a.LifecyclePending && ctx.Value(pluginTransitionKey{}) != a.ID) {
		return PluginSurfaceBundle{}, ErrAppNotFound
	}
	b, e := m.bundle(ctx, a.AppKey, false)
	if e != nil || b.Digest != *a.PluginDigest {
		// O watcher/trigger aplica upgrade sem expansão; até lá UI fica indisponível.
		if !m.Config.HotReload && !errors.Is(e, plugins.ErrBusy) {
			_ = m.quarantine(ctx, a)
		}
		return PluginSurfaceBundle{}, ErrAppNotFound
	}
	var origins []string
	var surfaces []AppSurfaceDeclaration
	if e = json.Unmarshal(a.ApprovedOrigins, &origins); e != nil {
		return PluginSurfaceBundle{}, e
	}
	if e = json.Unmarshal(a.PluginSurfaces, &surfaces); e != nil {
		return PluginSurfaceBundle{}, e
	}
	for _, p := range permissionList(a) {
		if !m.Config.PermissionAllowed(p) {
			return PluginSurfaceBundle{}, ErrPermissionDenied
		}
	}
	return PluginSurfaceBundle{Directory: b.Directory, Digest: b.Digest, Enabled: true, ApprovedOrigins: origins, Surfaces: surfaces}, nil
}

// quarantine mantém canais existentes e torna superfícies/host calls indisponíveis.
func (m *PluginManager) quarantine(ctx context.Context, a models.AppInstallation) error {
	if a.PluginState != "enabled" {
		return nil
	}
	if a.PluginDigest != nil {
		if e := storage.QuarantinePlugin(ctx, a.ID, *a.PluginDigest); e != nil {
			return e
		}
	}
	m.invalidate(ctx, a.ID)
	RecordAudit(ctx, AuditEntry{Action: ActionAppUpdate, EntityType: EntityApp, EntityID: &a.ID, Metadata: map[string]any{"state": "quarantined"}})
	return nil
}

// permissionList lê os grants aprovados, distintos das capacidades efetivas revogáveis.
func permissionList(a models.AppInstallation) []string {
	var p []string
	_ = json.Unmarshal(a.ApprovedPermissions, &p)
	return p
}

// emit entrega somente mudanças confirmadas aos leitores autorizados pelo adaptador.
func (m *PluginManager) emit(ctx context.Context, changes []AppChannelChange) {
	if m.Effects.Channels != nil {
		m.Effects.Channels(ctx, changes)
	}
}

// consentToken vincula ator e payload canônico; o segredo efêmero invalida previews no restart.
func (m *PluginManager) consentToken(actor string, p PluginPreview) string {
	p.Consent = ""
	data, _ := json.Marshal(p)
	mac := hmac.New(sha256.New, m.secret[:])
	mac.Write([]byte(actor))
	mac.Write([]byte{0})
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// consentEqual não revela correspondências parciais de um consentimento.
func consentEqual(a, b string) bool {
	return len(a) == 64 && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (m *PluginManager) invalidate(ctx context.Context, id string) {
	if m.Effects.Invalidate != nil {
		m.Effects.Invalidate(ctx, id)
	}
}
