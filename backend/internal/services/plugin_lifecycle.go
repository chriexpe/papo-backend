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
)

// Enable exige o mesmo preview assinado, inclusive ator, digest, settings e revisão.
func (m *PluginManager) Enable(ctx context.Context, actor, id string, o PluginOptions) (models.AppInstallation, error) {
	unlock, e := m.lock(ctx, id)
	if e != nil {
		return models.AppInstallation{}, e
	}
	defer unlock()
	if o.ExpiresAt == 0 {
		return models.AppInstallation{}, ErrPluginConsent
	}
	b, e := m.bundle(ctx, id, true)
	if e != nil {
		return models.AppInstallation{}, fmt.Errorf("%w: %v", ErrInvalidInput, e)
	}
	p, e := m.preview(ctx, actor, b, o)
	if e != nil {
		return models.AppInstallation{}, e
	}
	if !consentEqual(o.Consent, m.consentToken(actor, p)) {
		return models.AppInstallation{}, ErrPluginConsent
	}
	return m.apply(ctx, actor, b, p, true)
}

// apply persiste grants antes dos hooks; falhas de lifecycle desativam e removem canais.
func (m *PluginManager) apply(ctx context.Context, actor string, b pluginbundle.Bundle, p PluginPreview, enable bool) (models.AppInstallation, error) {
	var avatar *string
	for _, name := range []string{"icon.png", "icon.webp"} {
		if !has(b.Files, name) {
			continue
		}
		c := m.Config
		c.AssetBytes = c.IconBytes
		c.ReadBytes = c.IconBytes
		data, e := pluginbundle.ReadAsset(b.Directory, name, 0, c.IconBytes, c)
		if e != nil {
			return models.AppInstallation{}, e
		}
		mime := "image/png"
		if name == "icon.webp" {
			mime = "image/webp"
		}
		sha, _, e := StoreMediaFromBytes(ctx, data, mime)
		if e != nil {
			return models.AppInstallation{}, e
		}
		avatar = &sha
		break
	}
	// Revalidação imediatamente antes do commit evita aprovar assets alterados após preview.
	fresh, e := m.bundle(ctx, b.Manifest.ID, false)
	if errors.Is(e, plugins.ErrBusy) {
		return models.AppInstallation{}, e
	}
	if e != nil || fresh.Digest != p.Digest {
		return models.AppInstallation{}, ErrPluginConsent
	}
	a, e := storage.SavePlugin(ctx, storage.PluginApproval{Key: p.ID, Name: p.Name, Actor: actor, Digest: p.Digest, Version: p.Version, Settings: jsonValue(p.Settings), Origins: jsonValue(p.Origins), Permissions: jsonValue(p.Permissions), Surfaces: jsonValue(p.Surfaces), ExplicitScopes: jsonValue(p.ExplicitScopes), MemoryMB: p.MaxMemoryMB, Capabilities: capabilities(p.Permissions), Scopes: p.Scopes, ExpectedRevision: p.Revision, Avatar: avatar})
	if errors.Is(e, storage.ErrUniqueViolation) {
		return a, ErrPluginConsent
	}
	if e != nil {
		return a, e
	}
	m.Host.Reset(a.ID)
	transitionCtx := context.WithValue(ctx, pluginTransitionKey{}, a.ID)
	changes, e := ReconcileAppChannels(transitionCtx, actor, a.ID, p.Surfaces)
	if e == nil {
		m.emit(ctx, changes)
	}
	if e == nil && enable {
		_, e = m.invoke(transitionCtx, a, b, "on_enable", nil)
	}
	if e == nil {
		e = storage.FinishPlugin(ctx, a.ID, a.PluginRevision)
	}
	if e != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		changes, removeErr := DeleteAppChannels(cleanup, actor, a.ID)
		m.emit(cleanup, changes)
		stateErr := storage.SetPluginState(cleanup, a.ID, "disabled")
		return a, errors.Join(e, removeErr, stateErr)
	}
	RecordAudit(ctx, AuditEntry{ActorID: actor, Action: ActionAppUpdate, EntityType: EntityApp, EntityID: &a.ID, Metadata: map[string]any{"plugin_digest": p.Digest, "state": "enabled", "permissions": p.Permissions, "scopes": p.Scopes}})
	return storage.GetApp(ctx, a.ID)
}

// Disable bloqueia novas invocações antes de on_disable e faz cleanup mesmo após trap.
func (m *PluginManager) Disable(ctx context.Context, actor, id string) error {
	unlock, e := m.lock(ctx, id)
	if e != nil {
		return e
	}
	defer unlock()
	a, e := storage.FindPlugin(ctx, id)
	if e == storage.ErrNotFound {
		return ErrAppNotFound
	}
	if e != nil {
		return e
	}
	return m.disableLocked(ctx, a, actor, false)
}

// disableLocked nunca executa bytes diferentes do digest aprovado durante remoção.
func (m *PluginManager) disableLocked(ctx context.Context, a models.AppInstallation, actor string, removed bool) error {
	var callbackErr error
	if a.PluginState == "enabled" {
		if b, e := m.bundle(ctx, a.AppKey, false); e == nil && a.PluginDigest != nil && b.Digest == *a.PluginDigest {
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
	changes, channelErr := DeleteAppChannels(cleanup, actor, a.ID)
	m.emit(cleanup, changes)
	m.Host.Reset(a.ID)
	RecordAudit(cleanup, AuditEntry{ActorID: actor, Action: ActionAppUpdate, EntityType: EntityApp, EntityID: &a.ID, Metadata: map[string]any{"state": state, "on_disable_failed": callbackErr != nil}})
	return errors.Join(callbackErr, stateErr, channelErr)
}

// reloadLocked aplica somente mudanças sem novos grants, origens ou escopos.
func (m *PluginManager) reloadLocked(ctx context.Context, a models.AppInstallation) error {
	b, e := m.bundle(ctx, a.AppKey, true)
	if e != nil {
		_ = m.quarantine(ctx, a)
		return plugins.ErrQuarantined
	}
	old := permissionList(a)
	o := PluginOptions{Permissions: &old, MaxMemoryMB: min(a.MaxMemoryMB, b.Manifest.Runtime.MaxMemoryMB)}
	p, e := m.preview(ctx, "", b, o)
	var origins []string
	_ = json.Unmarshal(a.ApprovedOrigins, &origins)
	scopes, scopeErr := storage.PluginScopes(ctx, a.ID)
	// Expansão é definida pelo pedido novo, mesmo que o admin antes tenha concedido um subconjunto.
	var requested []string
	for _, v := range b.Manifest.Permissions.Request {
		if m.Config.PermissionAllowed(v) {
			requested = append(requested, v)
		}
	}
	if e != nil || scopeErr != nil || expanded(requested, old) || expanded(p.Origins, origins) || expanded(p.Scopes, scopes) {
		_ = m.quarantine(ctx, a)
		return ErrPluginConsent
	}
	_, e = m.apply(ctx, "", b, p, true)
	return e
}

// Patch atualiza settings/memória habilitados; escopos/origens novos exigem novo consentimento.
func (m *PluginManager) Patch(ctx context.Context, actor, id string, o PluginOptions) (models.AppInstallation, error) {
	unlock, e := m.lock(ctx, id)
	if e != nil {
		return models.AppInstallation{}, e
	}
	defer unlock()
	a, e := storage.FindPlugin(ctx, id)
	if e != nil {
		return a, e
	}
	if a.PluginState != "enabled" || a.LifecyclePending {
		return a, plugins.ErrQuarantined
	}
	b, e := m.bundle(ctx, id, true)
	if e != nil {
		return a, e
	}
	if a.PluginDigest == nil || b.Digest != *a.PluginDigest {
		_ = m.quarantine(ctx, a)
		return a, ErrPluginConsent
	}
	if o.Permissions != nil || o.Scopes != nil {
		return a, ErrInvalidInput
	}
	granted := permissionList(a)
	o.Permissions = &granted
	p, e := m.preview(ctx, actor, b, o)
	if e != nil {
		return a, e
	}
	scopes, e := storage.PluginScopes(ctx, a.ID)
	if e != nil {
		return a, e
	}
	var origins []string
	_ = json.Unmarshal(a.ApprovedOrigins, &origins)
	changed := string(jsonValue(sorted(scopes))) != string(jsonValue(p.Scopes)) || string(jsonValue(sorted(origins))) != string(jsonValue(p.Origins))
	if changed && !consentEqual(o.Consent, m.consentToken(actor, p)) {
		return a, ErrPluginConsent
	}
	return m.apply(ctx, actor, b, p, false)
}

// Trigger aplica upgrades na fronteira e entrega apenas on_trigger nesta rodada.
func (m *PluginManager) Trigger(parent context.Context, id string, input json.RawMessage) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	unlock, e := m.lock(ctx, id)
	if e != nil {
		return nil, e
	}
	defer unlock()
	a, e := storage.FindPlugin(ctx, id)
	if e == storage.ErrNotFound {
		return nil, ErrAppNotFound
	}
	if e != nil {
		return nil, e
	}
	if a.PluginState != "enabled" || a.LifecyclePending {
		return nil, plugins.ErrQuarantined
	}
	b, e := m.bundle(ctx, id, false)
	if errors.Is(e, plugins.ErrBusy) {
		return nil, e
	}
	if e != nil {
		_ = m.quarantine(ctx, a)
		return nil, plugins.ErrQuarantined
	}
	if a.PluginDigest == nil || b.Digest != *a.PluginDigest {
		if !m.Config.HotReload {
			_ = m.quarantine(ctx, a)
			return nil, ErrPluginConsent
		}
		if e = m.reloadLocked(ctx, a); e != nil {
			return nil, e
		}
		a, e = storage.FindPlugin(ctx, id)
		if e != nil {
			return nil, e
		}
		b, e = m.bundle(ctx, id, false)
		if e != nil {
			return nil, e
		}
	}
	out, e := m.invoke(ctx, a, b, "on_trigger", input)
	if errors.Is(e, plugins.ErrQuarantined) {
		_ = m.quarantine(ctx, a)
	}
	return out, e
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
	envelope := jsonValue(map[string]any{"type": event, "settings": json.RawMessage(a.PluginSettings), "input": input})
	out, e := m.Host.Invoke(plugins.WithMemoryLimit(ctx, a.MaxMemoryMB), a.ID, b.Module, envelope, func(c context.Context, data []byte) ([]byte, error) { return m.hostCall(c, a.ID, data) })
	if e == nil && event != "on_trigger" {
		var result struct {
			OK bool `json:"ok"`
		}
		if json.Unmarshal(out, &result) != nil || !result.OK {
			e = plugins.ErrExecution
		}
	}
	return out, e
}
