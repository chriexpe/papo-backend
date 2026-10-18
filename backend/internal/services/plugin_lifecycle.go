package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"papo/internal/models"
	"papo/internal/pluginbundle"
	"papo/internal/plugins"
	"papo/internal/storage"
	"papo/internal/utils"
)

// Enable exige o mesmo preview assinado, inclusive ator, digest, settings e
// revisão. Os bytes aprovados são copiados da pasta e conferidos contra o
// digest do preview: uma pasta alterada depois do preview é recusada.
func (m *PluginManager) Enable(ctx context.Context, actor, id string, o PluginOptions) (models.AppInstallation, error) {
	unlock, err := m.lock(ctx, id)
	if err != nil {
		return models.AppInstallation{}, err
	}
	defer unlock()
	if o.ExpiresAt == 0 {
		return models.AppInstallation{}, ErrPluginConsent
	}
	live, err := m.liveBundle(ctx, id, true)
	if err != nil {
		return models.AppInstallation{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	p, err := m.preview(ctx, actor, live, o)
	if err != nil {
		return models.AppInstallation{}, err
	}
	if !consentEqual(o.Consent, m.consentToken(actor, p)) {
		return models.AppInstallation{}, ErrPluginConsent
	}
	b, err := m.approve(ctx, id, p.Digest)
	if err != nil {
		return models.AppInstallation{}, err
	}
	return m.apply(ctx, actor, b, p, true)
}

// apply persiste grants a partir da cópia aprovada b antes dos hooks; falhas
// de lifecycle desativam e removem canais.
func (m *PluginManager) apply(ctx context.Context, actor string, b pluginbundle.Bundle, p PluginPreview, enable bool) (models.AppInstallation, error) {
	avatar, err := m.storeIcon(ctx, b)
	if err != nil {
		return models.AppInstallation{}, err
	}
	a, err := storage.SavePlugin(ctx, storage.PluginApproval{
		Key:              p.ID,
		Name:             p.Name,
		Actor:            actor,
		Digest:           p.Digest,
		Version:          p.Version,
		Settings:         jsonValue(p.Settings),
		Origins:          jsonValue(p.Origins),
		Permissions:      jsonValue(p.Permissions),
		Surfaces:         jsonValue(p.Surfaces),
		ExplicitScopes:   jsonValue(p.ExplicitScopes),
		MemoryMB:         p.MaxMemoryMB,
		StorageQuota:     p.StorageQuotaOverrides,
		Capabilities:     capabilities(p.Permissions),
		Scopes:           p.Scopes,
		ExpectedRevision: p.Revision,
		Avatar:           avatar,
	})
	if errors.Is(err, storage.ErrUniqueViolation) {
		return a, ErrPluginConsent
	}
	if err != nil {
		return a, err
	}
	m.invalidate(ctx, a.ID)
	m.Host.Reset(a.ID)
	transitionCtx := context.WithValue(ctx, pluginTransitionKey{}, a.ID)
	changes, err := ReconcileAppChannels(transitionCtx, actor, a.ID, p.Surfaces)
	if err == nil {
		m.emit(ctx, changes)
	}
	if err == nil && enable {
		_, err = m.invoke(transitionCtx, a, b, "on_enable", nil)
	}
	if err == nil {
		err = storage.FinishPlugin(ctx, a.ID, a.PluginRevision)
	}
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		m.invalidate(cleanup, a.ID)
		changes, removeErr := DeleteAppChannels(cleanup, actor, a.ID)
		m.emit(cleanup, changes)
		stateErr := storage.SetPluginState(cleanup, a.ID, "disabled")
		return a, errors.Join(err, removeErr, stateErr)
	}
	RecordAudit(ctx, AuditEntry{
		ActorID:    actor,
		Action:     ActionAppUpdate,
		EntityType: EntityApp,
		EntityID:   &a.ID,
		Metadata: map[string]any{
			"plugin_digest": p.Digest,
			"state":         "enabled",
			"permissions":   p.Permissions,
			"scopes":        p.Scopes,
		},
	})
	return storage.GetApp(ctx, a.ID)
}

// storeIcon grava icon.png ou icon.webp da cópia aprovada como avatar do app.
func (m *PluginManager) storeIcon(ctx context.Context, b pluginbundle.Bundle) (*string, error) {
	for _, name := range []string{"icon.png", "icon.webp"} {
		if !has(b.Files, name) {
			continue
		}
		c := m.Config
		c.AssetBytes = c.IconBytes
		c.ReadBytes = c.IconBytes
		data, err := pluginbundle.ReadAsset(b.Directory, name, 0, c.IconBytes, c)
		if err != nil {
			return nil, err
		}
		mime := "image/png"
		if name == "icon.webp" {
			mime = "image/webp"
		}
		sha, _, err := StoreMediaFromBytes(ctx, data, mime)
		if err != nil {
			return nil, err
		}
		return &sha, nil
	}
	return nil, nil
}

// Disable bloqueia novas invocações antes de on_disable e faz cleanup mesmo após trap.
func (m *PluginManager) Disable(ctx context.Context, actor, id string) error {
	unlock, err := m.lock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	a, err := storage.FindPlugin(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrAppNotFound
	}
	if err != nil {
		return err
	}
	return m.disableLocked(ctx, a, actor, false)
}

// disableLocked executa on_disable somente com os bytes aprovados e fecha a
// autoridade mesmo quando o callback falha.
func (m *PluginManager) disableLocked(ctx context.Context, a models.AppInstallation, actor string, removed bool) error {
	var callbackErr error
	if a.PluginState == "enabled" {
		if b, err := m.snapshot(ctx, a); err == nil {
			_, callbackErr = m.invoke(ctx, a, b, "on_disable", nil)
		}
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	state := "disabled"
	if removed {
		state = "removed"
	}
	// Fecha autoridade antes de notificar exclusões; nenhum timeout deixa grants ativos.
	stateErr := storage.SetPluginState(cleanup, a.ID, state)
	if stateErr == nil {
		m.invalidate(cleanup, a.ID)
	}
	changes, channelErr := DeleteAppChannels(cleanup, actor, a.ID)
	m.emit(cleanup, changes)
	m.Host.Reset(a.ID)
	RecordAudit(cleanup, AuditEntry{
		ActorID:    actor,
		Action:     ActionAppUpdate,
		EntityType: EntityApp,
		EntityID:   &a.ID,
		Metadata:   map[string]any{"state": state, "on_disable_failed": callbackErr != nil},
	})
	return errors.Join(callbackErr, stateErr, channelErr)
}

// reloadLocked aprova a versão atual da pasta somente quando ela não pede
// permissões, origens ou escopos novos; caso contrário, põe em quarentena.
func (m *PluginManager) reloadLocked(ctx context.Context, a models.AppInstallation) error {
	live, err := m.liveBundle(ctx, a.AppKey, true)
	if err != nil {
		_ = m.quarantine(ctx, a)
		return plugins.ErrQuarantined
	}
	old := permissionList(a)
	o := PluginOptions{Permissions: &old, MaxMemoryMB: min(a.MaxMemoryMB, live.Manifest.Runtime.MaxMemoryMB)}
	p, err := m.preview(ctx, "", live, o)
	var origins []string
	_ = json.Unmarshal(a.ApprovedOrigins, &origins)
	scopes, scopeErr := storage.PluginScopes(ctx, a.ID)
	// Expansão é definida pelo pedido novo, mesmo que o admin antes tenha concedido um subconjunto.
	var requested []string
	for _, v := range live.Manifest.Permissions.Request {
		if m.Config.PermissionAllowed(v) {
			requested = append(requested, v)
		}
	}
	if err != nil || scopeErr != nil || expanded(requested, old) || expanded(p.Origins, origins) || expanded(p.Scopes, scopes) {
		_ = m.quarantine(ctx, a)
		return ErrPluginConsent
	}
	b, err := m.approve(ctx, a.AppKey, p.Digest)
	if err != nil {
		_ = m.quarantine(ctx, a)
		return ErrPluginConsent
	}
	_, err = m.apply(ctx, "", b, p, true)
	return err
}

// Patch atualiza settings, memória e quotas da versão aprovada; escopos ou
// origens novos exigem novo consentimento.
func (m *PluginManager) Patch(ctx context.Context, actor, id string, o PluginOptions) (models.AppInstallation, error) {
	unlock, err := m.lock(ctx, id)
	if err != nil {
		return models.AppInstallation{}, err
	}
	defer unlock()
	a, err := storage.FindPlugin(ctx, id)
	if err != nil {
		return a, err
	}
	if a.PluginState != "enabled" || a.LifecyclePending {
		return a, plugins.ErrQuarantined
	}
	if o.Permissions != nil || o.Scopes != nil {
		return a, ErrInvalidInput
	}
	b, err := m.snapshot(ctx, a)
	if err != nil {
		return a, err
	}
	granted := permissionList(a)
	o.Permissions = &granted
	p, err := m.preview(ctx, actor, b, o)
	if err != nil {
		return a, err
	}
	scopes, err := storage.PluginScopes(ctx, a.ID)
	if err != nil {
		return a, err
	}
	var origins []string
	_ = json.Unmarshal(a.ApprovedOrigins, &origins)
	changed := string(jsonValue(sorted(scopes))) != string(jsonValue(p.Scopes)) ||
		string(jsonValue(sorted(origins))) != string(jsonValue(p.Origins))
	if changed && !consentEqual(o.Consent, m.consentToken(actor, p)) {
		return a, ErrPluginConsent
	}
	return m.apply(ctx, actor, b, p, false)
}

// Trigger entrega on_trigger, chamado pela gestão.
func (m *PluginManager) Trigger(parent context.Context, id string, input json.RawMessage) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	return m.callEvent(ctx, id, "on_trigger", input)
}

// callEvent serializa as invocações da instalação e executa a versão aprovada.
func (m *PluginManager) callEvent(ctx context.Context, id, event string, input json.RawMessage) ([]byte, error) {
	unlock, err := m.lock(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	a, err := storage.FindPlugin(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, ErrAppNotFound
	}
	if err != nil {
		return nil, err
	}
	if a.PluginState != "enabled" || a.LifecyclePending {
		return nil, plugins.ErrQuarantined
	}
	b, err := m.snapshot(ctx, a)
	if err != nil {
		if transientPluginError(err) {
			return nil, err
		}
		utils.Errorf("plugins: cópia aprovada de %s indisponível: %v", a.AppKey, err)
		return nil, plugins.ErrExecution
	}
	out, err := m.invoke(ctx, a, b, event, input)
	if errors.Is(err, plugins.ErrQuarantined) {
		_ = m.quarantine(ctx, a)
	}
	return out, err
}

// invoke entrega settings em cada envelope; globals de guest nunca são persistência.
func (m *PluginManager) invoke(ctx context.Context, a models.AppInstallation, b pluginbundle.Bundle, event string, input json.RawMessage) ([]byte, error) {
	if a.PluginDigest == nil || b.Digest != *a.PluginDigest {
		return nil, ErrPluginConsent
	}
	for _, p := range permissionList(a) {
		if !m.Config.PermissionAllowed(p) {
			return nil, ErrPermissionDenied
		}
	}
	if event != "on_interaction" && event != "on_ui_call" {
		ctx = context.WithValue(ctx, appHumanKey{}, false)
	}
	envelope := jsonValue(map[string]any{"type": event, "settings": json.RawMessage(a.PluginSettings), "input": input})
	call := func(c context.Context, data []byte) ([]byte, error) { return m.hostCall(c, a.ID, b, data) }
	out, err := m.Host.Invoke(plugins.WithMemoryLimit(ctx, a.MaxMemoryMB), a.ID, b.Code(), envelope, call)
	if err == nil && event != "on_trigger" {
		var result struct {
			OK bool `json:"ok"`
		}
		if json.Unmarshal(out, &result) != nil || !result.OK {
			err = plugins.ErrExecution
		}
	}
	return out, err
}
