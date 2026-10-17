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
	Invite     func(context.Context, models.AppInvite)
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

// PluginManager liga descoberta, consentimento, runtime e superfícies.
//
// A pasta em PLUGINS_DIR é apenas a origem de uma versão: ao habilitar (ou no
// hot reload), os bytes aprovados são copiados para PLUGINS_DIR/.approved/
// <digest>/<id> e carregados uma vez em memória. Toda execução, asset e UI usa
// essa cópia; somente Rescan/Watch comparam a pasta viva com o digest aprovado
// para aplicar hot reload ou quarentena.
type PluginManager struct {
	Config  pluginbundle.Config
	Host    plugins.PluginHost
	Effects PluginEffects
	secret  [32]byte

	mu        sync.Mutex
	catalog   map[string]DiscoveredPlugin
	gates     map[string]*pluginGate
	snapshots map[string]pluginbundle.Bundle

	scan   chan struct{}
	limits *appLimiter
}

// NewPluginManager configura descoberta sem instanciar qualquer módulo.
func NewPluginManager(c pluginbundle.Config, h plugins.PluginHost, e PluginEffects) (*PluginManager, error) {
	m := &PluginManager{
		Config:    c,
		Host:      h,
		Effects:   e,
		catalog:   map[string]DiscoveredPlugin{},
		gates:     map[string]*pluginGate{},
		snapshots: map[string]pluginbundle.Bundle{},
		scan:      make(chan struct{}, 1),
		limits:    newAppLimiter(),
	}
	_, err := rand.Read(m.secret[:])
	return m, err
}

// validate compartilha ABI e teto de memória com o CLI e com o runtime real.
func (m *PluginManager) validate(ctx context.Context, b []byte, mb int) error {
	return m.Host.Validate(plugins.WithMemoryLimit(ctx, mb), b)
}

// lock serializa mudanças de versão e invocações da mesma instalação, com
// fila limitada.
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
	installs, err := storage.ListPluginInstallations(ctx)
	if err != nil {
		return nil, err
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

// Rescan descobre pastas e pacotes sem instanciar e reconcilia as instalações
// conhecidas: pasta removida desativa; pasta inválida, transição interrompida
// ou digest diferente do aprovado põem em quarentena (ou aplicam hot reload,
// quando habilitado e sem ampliação de permissões). Também garante a cópia
// aprovada de cada plugin habilitado e remove cópias que deixaram de ser usadas.
func (m *PluginManager) Rescan(ctx context.Context) error {
	select {
	case m.scan <- struct{}{}:
		defer func() { <-m.scan }()
	case <-ctx.Done():
		return ctx.Err()
	}
	next, err := m.discover(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.catalog = next
	m.mu.Unlock()

	installs, err := storage.ListPluginInstallations(ctx)
	if err != nil {
		return err
	}
	for _, a := range installs {
		if a.PluginState != "enabled" && a.PluginState != "quarantined" {
			continue
		}
		if err := m.reconcile(ctx, a, next); err != nil &&
			!errors.Is(err, ErrPluginConsent) && !errors.Is(err, plugins.ErrQuarantined) {
			return err
		}
	}
	return m.pruneSnapshots(ctx)
}

// discover lê PLUGINS_DIR: pastas com plugin.wasm e pacotes .papoplugin, que
// são extraídos uma vez. Entradas que começam com "." são ignoradas.
func (m *PluginManager) discover(ctx context.Context) (map[string]DiscoveredPlugin, error) {
	if err := os.MkdirAll(m.Config.Directory, 0750); err != nil {
		return nil, err
	}
	rootInfo, err := os.Lstat(m.Config.Directory)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalidInput
	}
	entries, err := os.ReadDir(m.Config.Directory)
	if err != nil {
		return nil, err
	}
	if len(entries) > m.Config.Files {
		return nil, fmt.Errorf("limite de candidatos excedido")
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
			archiveInfo, err := entry.Info()
			if err != nil {
				return nil, err
			}
			if archiveInfo.Size() > m.Config.FolderBytes+(1<<20) {
				next[name] = DiscoveredPlugin{ID: name, Error: "ZIP excede limite"}
				continue
			}
			// Pacotes já extraídos não substituem a pasta canônica nem a invalidam.
			if _, err = os.Lstat(filepath.Join(m.Config.Directory, strings.TrimSuffix(name, ".papoplugin"))); err == nil {
				continue
			}
			b, err := pluginbundle.Extract(ctx, full, m.Config.Directory, m.Config, m.validate)
			if err != nil {
				next[name] = DiscoveredPlugin{ID: name, Error: err.Error()}
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
		if _, err := os.Lstat(filepath.Join(full, "plugin.wasm")); os.IsNotExist(err) {
			continue
		}
		b, err := pluginbundle.Load(ctx, full, m.Config, m.validate)
		if err != nil {
			next[name] = DiscoveredPlugin{ID: name, Error: err.Error()}
			continue
		}
		v := b.Manifest
		next[name] = DiscoveredPlugin{ID: name, Manifest: &v, Digest: b.Digest}
	}
	return next, nil
}

// reconcile aplica a uma instalação habilitada ou em quarentena o resultado da
// descoberta.
func (m *PluginManager) reconcile(ctx context.Context, a models.AppInstallation, next map[string]DiscoveredPlugin) error {
	opCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	unlock, err := m.lock(opCtx, a.AppKey)
	if err != nil {
		return err
	}
	defer unlock()
	d, exists := next[a.AppKey]
	switch {
	case a.LifecyclePending:
		return m.quarantine(opCtx, a)
	case !exists:
		return m.disableLocked(opCtx, a, "", true)
	case d.Error != "":
		return m.quarantine(opCtx, a)
	case a.PluginDigest == nil || d.Digest != *a.PluginDigest:
		if m.Config.HotReload && a.PluginState == "enabled" {
			return m.reloadLocked(opCtx, a)
		}
		return m.quarantine(opCtx, a)
	case a.PluginState == "enabled":
		// Mesmo digest: garante a cópia aprovada (recriando-a a partir da
		// pasta, que tem exatamente os bytes aprovados) antes de servir.
		if _, err := m.snapshot(opCtx, a); err == nil {
			return nil
		}
		if _, err := m.approve(opCtx, a.AppKey, *a.PluginDigest); err != nil {
			utils.Errorf("plugins: cópia aprovada de %s indisponível: %v", a.AppKey, err)
			return m.quarantine(opCtx, a)
		}
	}
	return nil
}

// pruneSnapshots descarta cópias aprovadas (em disco e em memória) que não
// pertencem a nenhuma instalação habilitada.
func (m *PluginManager) pruneSnapshots(ctx context.Context) error {
	installs, err := storage.ListPluginInstallations(ctx)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, a := range installs {
		if a.PluginState == "enabled" && a.PluginDigest != nil {
			keep[*a.PluginDigest] = true
		}
	}
	m.mu.Lock()
	for digest := range m.snapshots {
		if !keep[digest] {
			delete(m.snapshots, digest)
		}
	}
	m.mu.Unlock()
	return pluginbundle.PruneSnapshots(m.Config.Directory, keep)
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
			if err := m.Rescan(ctx); err != nil && ctx.Err() == nil {
				utils.Errorf("plugins: rescan: %v", err)
			}
		}
	}
}

// liveBundle lê e valida a pasta viva do plugin id. Usado somente por
// descoberta, preview e aprovação, nunca por execução ou UI.
func (m *PluginManager) liveBundle(ctx context.Context, id string, validate bool) (pluginbundle.Bundle, error) {
	if !pluginID(id) {
		return pluginbundle.Bundle{}, ErrInvalidInput
	}
	var v pluginbundle.Validator
	if validate {
		v = m.validate
	}
	return pluginbundle.Load(ctx, filepath.Join(m.Config.Directory, id), m.Config, v)
}

// approve copia a pasta viva de key para a cópia aprovada de digest want.
// Retorna ErrPluginConsent quando a pasta mudou depois do preview.
func (m *PluginManager) approve(ctx context.Context, key, want string) (pluginbundle.Bundle, error) {
	if !pluginID(key) {
		return pluginbundle.Bundle{}, ErrInvalidInput
	}
	src := filepath.Join(m.Config.Directory, key)
	b, err := pluginbundle.Approve(ctx, m.Config.Directory, src, want, m.Config, m.validate)
	if errors.Is(err, pluginbundle.ErrDigestMismatch) {
		return b, ErrPluginConsent
	}
	if err != nil {
		return b, err
	}
	m.mu.Lock()
	m.snapshots[b.Digest] = b
	m.mu.Unlock()
	return b, nil
}

// snapshot devolve a cópia aprovada do digest da instalação, carregando-a do
// disco somente na primeira vez.
func (m *PluginManager) snapshot(ctx context.Context, a models.AppInstallation) (pluginbundle.Bundle, error) {
	if a.PluginDigest == nil {
		return pluginbundle.Bundle{}, ErrAppNotFound
	}
	digest := *a.PluginDigest
	m.mu.Lock()
	b, ok := m.snapshots[digest]
	m.mu.Unlock()
	if ok {
		return b, nil
	}
	b, err := pluginbundle.OpenSnapshot(ctx, m.Config.Directory, digest, a.AppKey, m.Config, m.validate)
	if err != nil {
		return b, err
	}
	m.mu.Lock()
	m.snapshots[digest] = b
	m.mu.Unlock()
	return b, nil
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

// Resolve implementa PluginSurfaceSource a partir da cópia aprovada: o
// diretório devolvido é imutável e nunca a pasta viva.
func (m *PluginManager) Resolve(ctx context.Context, id string) (PluginSurfaceBundle, error) {
	a, err := storage.GetApp(ctx, id)
	if err != nil {
		return PluginSurfaceBundle{}, err
	}
	if a.Kind != "plugin" || a.RevokedAt != nil || a.PluginState != "enabled" || a.PluginDigest == nil ||
		(a.LifecyclePending && ctx.Value(pluginTransitionKey{}) != a.ID) {
		return PluginSurfaceBundle{}, ErrAppNotFound
	}
	b, err := m.snapshot(ctx, a)
	if err != nil {
		if transientPluginError(err) {
			return PluginSurfaceBundle{}, err
		}
		utils.Errorf("plugins: cópia aprovada de %s indisponível: %v", a.AppKey, err)
		return PluginSurfaceBundle{}, ErrAppNotFound
	}
	var origins []string
	var surfaces []AppSurfaceDeclaration
	if err = json.Unmarshal(a.ApprovedOrigins, &origins); err != nil {
		return PluginSurfaceBundle{}, err
	}
	if err = json.Unmarshal(a.PluginSurfaces, &surfaces); err != nil {
		return PluginSurfaceBundle{}, err
	}
	for _, p := range permissionList(a) {
		if !m.Config.PermissionAllowed(p) {
			return PluginSurfaceBundle{}, ErrPermissionDenied
		}
	}
	return PluginSurfaceBundle{
		AppKey:          a.AppKey,
		Directory:       b.Directory,
		Digest:          b.Digest,
		Enabled:         true,
		ApprovedOrigins: origins,
		Surfaces:        surfaces,
		UserActions:     b.Manifest.UserActions,
	}, nil
}

// quarantine mantém canais existentes e torna superfícies/host calls indisponíveis.
func (m *PluginManager) quarantine(ctx context.Context, a models.AppInstallation) error {
	if a.PluginState != "enabled" {
		return nil
	}
	if a.PluginDigest != nil {
		if err := storage.QuarantinePlugin(ctx, a.ID, *a.PluginDigest); err != nil {
			return err
		}
	}
	m.invalidate(ctx, a.ID)
	RecordAudit(ctx, AuditEntry{
		Action:     ActionAppUpdate,
		EntityType: EntityApp,
		EntityID:   &a.ID,
		Metadata:   map[string]any{"state": "quarantined"},
	})
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

// invalidate encerra tickets e sessões UI da instalação e avisa os clientes.
func (m *PluginManager) invalidate(ctx context.Context, id string) {
	if err := storage.RevokeAppUI(ctx, id); err != nil {
		utils.Errorf("falha ao revogar sessões UI: %v", err)
	}
	if m.Effects.Invalidate != nil {
		m.Effects.Invalidate(ctx, id)
	}
}

// transientPluginError não confunde saturação/cancelamento do pedido com artefato inválido.
func transientPluginError(err error) bool {
	return errors.Is(err, plugins.ErrBusy) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
