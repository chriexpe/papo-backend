package services

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"papo/internal/models"
	"papo/internal/pluginbundle"
	"papo/internal/storage"
	"papo/internal/utils"
)

// pluginRequest não aceita identidade nem namespace fornecido pelo guest.
type pluginRequest struct {
	IDs               []string              `json:"ids"`
	Components        *[][]models.AppButton `json:"components"`
	ToUserID          string                `json:"to_user_id"`
	Text              string                `json:"text"`
	Data              json.RawMessage       `json:"data"`
	Method            string                `json:"method"`
	Key               string                `json:"key"`
	Value             json.RawMessage       `json:"value"`
	Path              string                `json:"path"`
	Offset            int64                 `json:"offset"`
	Length            int64                 `json:"length"`
	ChannelID         string                `json:"channel_id"`
	MessageID         string                `json:"message_id"`
	Content           string                `json:"content"`
	ExternalMessageID string                `json:"external_message_id"`
	Embeds            []EmbedInput          `json:"embeds"`
}

// hostCall recarrega estado e grants antes de cada efeito. b é a cópia
// aprovada da invocação em andamento; uma aprovação nova no meio da chamada
// encerra a autoridade dela.
func (m *PluginManager) hostCall(ctx context.Context, id string, b pluginbundle.Bundle, data []byte) ([]byte, error) {
	var req pluginRequest
	if json.Unmarshal(data, &req) != nil {
		return pluginResponse(nil, ErrInvalidInput)
	}
	a, err := activeApp(ctx, id)
	if err != nil {
		return pluginResponse(nil, err)
	}
	if a.Kind != "plugin" || a.PluginState != "enabled" || (a.LifecyclePending && ctx.Value(pluginTransitionKey{}) != a.ID) {
		return pluginResponse(nil, ErrPermissionDenied)
	}
	if a.PluginDigest == nil || b.Digest != *a.PluginDigest {
		return pluginResponse(nil, ErrPermissionDenied)
	}
	for _, p := range permissionList(a) {
		if !m.Config.PermissionAllowed(p) {
			return pluginResponse(nil, ErrPermissionDenied)
		}
	}
	var result any
	switch req.Method {
	case "users.resolve":
		result, err = m.ResolveUsers(ctx, a.ID, req.IDs)
	case "asset.read":
		var content []byte
		content, err = pluginbundle.ReadAsset(b.Directory, req.Path, req.Offset, req.Length, m.Config)
		if err == nil {
			result = map[string]any{"data": content, "offset": req.Offset, "length": len(content)}
		} else {
			err = ErrInvalidInput
		}
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
			if req.Method == "kv.put" && (!json.Valid(req.Value)) {
				return pluginResponse(nil, ErrInvalidInput)
			}
			result, err = storage.MutateAppStorage(ctx, id, req.Key, req.Value, req.Method == "kv.delete")
		}
	case "invite.send":
		result, err = m.SendAppInvite(ctx, a, req.ToUserID, req.Text, req.Data)
	case "message.send":
		if req.Components != nil {
			if err = ValidateAppButtons(ctx, id, *req.Components); err != nil {
				return pluginResponse(nil, err)
			}
		}

		frames := 0
		for _, embed := range req.Embeds {
			if embed.SourceType == "app" {
				frames++
			}
			if embed.SourceType != "app" && embed.SourceType != "custom" {
				return pluginResponse(nil, ErrInvalidEmbed)
			}
		}
		if frames > 1 {
			return pluginResponse(nil, ErrInvalidEmbed)
		}
		var msg models.MessageWithAttachment
		var created bool
		if req.Components != nil && len(*req.Components) > 0 {
			ctx = context.WithValue(ctx, appButtonsKey{}, true)
			ctx = storage.WithMessageComponents(ctx, *req.Components)
		}
		msg, created, err = CreatePluginMessage(ctx, id, req.ChannelID, req.ExternalMessageID, req.Content, "", nil, req.Embeds)
		if err == nil {
			comps, err := storage.AppComponents(ctx, []string{msg.ID})
			if err != nil {
				return pluginResponse(nil, err)
			}
			msg.Components = comps[msg.ID]
			result = msg
			if created && m.Effects.Created != nil {
				m.Effects.Created(ctx, msg, a.CreatedBy)
			}
		}
	case "message.edit":
		if req.Components != nil {
			if err = ValidateAppButtons(ctx, id, *req.Components); err != nil {
				return pluginResponse(nil, err)
			}
		}

		frames := 0
		for _, embed := range req.Embeds {
			if embed.SourceType == "app" {
				frames++
			}
			if embed.SourceType != "app" && embed.SourceType != "custom" {
				return pluginResponse(nil, ErrInvalidEmbed)
			}
		}
		if frames > 1 {
			return pluginResponse(nil, ErrInvalidEmbed)
		}
		if req.Components != nil {
			ctx = storage.WithMessageComponents(ctx, *req.Components)
		}
		var msg models.MessageWithAttachment
		msg, err = EditAppMessage(ctx, a, req.ChannelID, req.MessageID, req.Content, req.Embeds)
		if err == nil {
			comps, err := storage.AppComponents(ctx, []string{msg.ID})
			if err != nil {
				return pluginResponse(nil, err)
			}
			msg.Components = comps[msg.ID]
			result = msg
			if m.Effects.Edited != nil {
				m.Effects.Edited(ctx, msg, a.CreatedBy)
			}
		}
	case "message.delete":
		var channel string
		channel, err = DeleteAppMessage(ctx, a, req.ChannelID, req.MessageID)
		if err == nil && m.Effects.Deleted != nil {
			m.Effects.Deleted(ctx, channel, req.MessageID)
		}
	case "log":
		if len(req.Content) > 4096 {
			return pluginResponse(nil, ErrInvalidInput)
		}
		utils.Infof("plugin=%s %s", a.AppKey, req.Content)
	case "time.now":
		result = time.Now().UTC().Format(time.RFC3339Nano)
	default:
		err = ErrInvalidInput
	}
	return pluginResponse(result, err)
}

// pluginResponse mantém erros de autorização dentro do protocolo do guest.
func pluginResponse(result any, err error) ([]byte, error) {
	if err == nil {
		return json.Marshal(map[string]any{"ok": true, "result": result})
	}
	code := "internal"
	switch {
	case errors.Is(err, ErrPermissionDenied), errors.Is(err, ErrAppNotFound), errors.Is(err, ErrAppUnauthorized):
		code = "forbidden"
	case errors.Is(err, ErrInvalidInput), errors.Is(err, ErrInvalidEmbed):
		code = "invalid_param"
	case errors.Is(err, storage.ErrNotFound):
		code = "not_found"
	case errors.Is(err, ErrAppRateLimit):
		code = "rate_limited"
	case errors.Is(err, storage.ErrAppStorageQuota):
		code = "quota_exceeded"
	}
	return json.Marshal(map[string]any{"ok": false, "error": map[string]string{"code": code}})
}
