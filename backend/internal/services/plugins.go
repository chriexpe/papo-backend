package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"unicode/utf8"

	"papo/internal/models"
	"papo/internal/plugins"
	"papo/internal/storage"
)

// PluginMessagePublisher publica somente mensagens novas pelo fluxo comum do REST.
type PluginMessagePublisher func(models.MessageWithAttachment, string)

// UploadPlugin valida antes de armazenar; SHA-256 público é separado do HMAC da mídia.
func UploadPlugin(ctx context.Context, host plugins.PluginHost, actor, id string, data []byte) (string, error) {
	a, err := activeApp(ctx, id)
	if err != nil {
		return "", err
	}
	if a.Kind != "wasm" {
		return "", ErrInvalidInput
	}
	if err = host.Validate(ctx, data); err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	text := hex.EncodeToString(digest[:])
	sha, _, err := StoreMediaFromBytes(ctx, data, "application/wasm")
	if err != nil {
		return "", err
	}
	if err = storage.SetAppArtifact(ctx, id, sha, text); err != nil {
		return "", err
	}
	host.Reset(id)
	RecordAudit(ctx, AuditEntry{ActorID: actor, Action: ActionAppUpdate, EntityType: EntityApp, EntityID: &id, Metadata: map[string]any{"artifact_digest": text}})
	return text, nil
}

// InvokePlugin carrega uma instalação ativa e verifica a integridade antes de executar.
func InvokePlugin(ctx context.Context, host plugins.PluginHost, id string, input []byte, publish PluginMessagePublisher) ([]byte, error) {
	a, err := activeApp(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Kind != "wasm" || a.ArtifactSHA == nil || a.ArtifactDigest == nil {
		return nil, ErrInvalidInput
	}
	f, err := os.Open(MediaBlobPath(*a.ArtifactSHA))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, plugins.MaxArtifact+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if len(data) > plugins.MaxArtifact || hex.EncodeToString(sum[:]) != *a.ArtifactDigest {
		return nil, plugins.ErrInvalidArtifact
	}
	return host.Invoke(ctx, id, data, input, func(callCtx context.Context, request []byte) ([]byte, error) {
		return pluginCall(callCtx, id, request, publish)
	})
}

// pluginRequest não aceita identidade do remetente nem namespace fornecido pelo guest.
type pluginRequest struct {
	Method            string          `json:"method"`
	Key               string          `json:"key"`
	Value             json.RawMessage `json:"value"`
	ChannelID         string          `json:"channel_id"`
	Content           string          `json:"content"`
	ExternalMessageID string          `json:"external_message_id"`
}

// pluginCall recarrega a autorização em cada chamada, inclusive após efeitos anteriores.
func pluginCall(ctx context.Context, id string, data []byte, publish PluginMessagePublisher) ([]byte, error) {
	var req pluginRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return pluginResponse(nil, ErrInvalidInput)
	}
	a, err := activeApp(ctx, id)
	if err != nil {
		return pluginResponse(nil, err)
	}
	if a.Kind != "wasm" {
		return pluginResponse(nil, ErrPermissionDenied)
	}
	var result any
	switch req.Method {
	case "kv.get", "kv.put", "kv.delete":
		if !a.Capabilities.PrivateStorage {
			return pluginResponse(nil, ErrPermissionDenied)
		}
		if len(req.Key) == 0 || len(req.Key) > 200 || !utf8.ValidString(req.Key) {
			return pluginResponse(nil, ErrInvalidInput)
		}
		if req.Method == "kv.get" {
			result, err = storage.GetAppStorage(ctx, id, req.Key)
		} else {
			if req.Method == "kv.put" && (len(req.Value) > 16384 || !json.Valid(req.Value)) {
				return pluginResponse(nil, ErrInvalidInput)
			}
			result, err = storage.MutateAppStorage(ctx, id, req.Key, req.Value, req.Method == "kv.delete")
		}
	case "message.send":
		var m models.MessageWithAttachment
		var created bool
		m, created, err = CreateAppMessage(ctx, a, req.ChannelID, req.ExternalMessageID, req.Content, "", nil, nil)
		if err == nil {
			result = m
			if created && publish != nil {
				publish(m, a.CreatedBy)
			}
		}
	default:
		err = ErrInvalidInput
	}
	return pluginResponse(result, err)
}

// pluginResponse usa códigos pequenos sem revelar detalhes internos do banco ou runtime.
func pluginResponse(result any, err error) ([]byte, error) {
	if err == nil {
		return json.Marshal(map[string]any{"ok": true, "result": result})
	}
	code := "internal"
	switch {
	case errors.Is(err, ErrPermissionDenied), errors.Is(err, ErrAppNotFound), errors.Is(err, ErrAppUnauthorized):
		code = "forbidden"
	case errors.Is(err, ErrInvalidInput):
		code = "invalid_param"
	case errors.Is(err, storage.ErrNotFound):
		code = "not_found"
	case errors.Is(err, storage.ErrAppStorageQuota):
		code = "quota_exceeded"
	}
	return json.Marshal(map[string]any{"ok": false, "error": map[string]string{"code": code}})
}
