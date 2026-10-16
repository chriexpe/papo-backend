package services

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"papo/internal/models"
	"papo/internal/pluginbundle"
	"papo/internal/plugins"
	"papo/internal/storage"
	"papo/internal/utils"
)

// pluginRequest não aceita identidade nem namespace fornecido pelo guest.
type pluginRequest struct {
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

// hostCall recarrega estado, grants e digest antes de cada efeito e leitura de asset.
func (m *PluginManager) hostCall(ctx context.Context, id string, data []byte) ([]byte, error) {
	var req pluginRequest
	if json.Unmarshal(data, &req) != nil {
		return pluginResponse(nil, ErrInvalidInput)
	}
	a, e := activeApp(ctx, id)
	if e != nil {
		return pluginResponse(nil, e)
	}
	if a.Kind != "plugin" || a.PluginState != "enabled" || (a.LifecyclePending && ctx.Value(pluginTransitionKey{}) != a.ID) {
		return pluginResponse(nil, ErrPermissionDenied)
	}
	b, e := m.bundle(ctx, a.AppKey, false)
	if e != nil || a.PluginDigest == nil || b.Digest != *a.PluginDigest {
		if !m.Config.HotReload && !errors.Is(e, plugins.ErrBusy) {
			_ = m.quarantine(ctx, a)
		}
		return pluginResponse(nil, ErrPermissionDenied)
	}
	for _, p := range permissionList(a) {
		if !m.Config.PermissionAllowed(p) {
			return pluginResponse(nil, ErrPermissionDenied)
		}
	}
	var result any
	switch req.Method {
	case "asset.read":
		var content []byte
		content, e = pluginbundle.ReadAsset(b.Directory, req.Path, req.Offset, req.Length, m.Config)
		if e == nil {
			result = map[string]any{"data": content, "offset": req.Offset, "length": len(content)}
		} else {
			e = ErrInvalidInput
		}
	case "kv.get", "kv.put", "kv.delete":
		if !a.Capabilities.PrivateStorage {
			return pluginResponse(nil, ErrPermissionDenied)
		}
		if len(req.Key) == 0 || len(req.Key) > 200 || !utf8.ValidString(req.Key) {
			return pluginResponse(nil, ErrInvalidInput)
		}
		if req.Method == "kv.get" {
			result, e = storage.GetAppStorage(ctx, id, req.Key)
		} else {
			if req.Method == "kv.put" && (len(req.Value) > 16384 || !json.Valid(req.Value)) {
				return pluginResponse(nil, ErrInvalidInput)
			}
			result, e = storage.MutateAppStorage(ctx, id, req.Key, req.Value, req.Method == "kv.delete")
		}
	case "invite.send":
		result, e = m.SendAppInvite(ctx, a, req.ToUserID, req.Text, req.Data)
	case "message.send":
		if req.Components != nil {
			if e = ValidateAppButtons(ctx, id, *req.Components); e != nil {
				return pluginResponse(nil, e)
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
		}
		msg, created, e = CreatePluginMessage(ctx, id, req.ChannelID, req.ExternalMessageID, req.Content, "", nil, req.Embeds)
		if e == nil && created && req.Components != nil {
			e = storage.SaveAppComponents(ctx, id, req.ChannelID, msg.ID, *req.Components)
		}
		if e == nil {
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
			if e = ValidateAppButtons(ctx, id, *req.Components); e != nil {
				return pluginResponse(nil, e)
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
		msg, e = EditAppMessage(ctx, a, req.ChannelID, req.MessageID, req.Content, req.Embeds)
		if e == nil && req.Components != nil {
			e = storage.SaveAppComponents(ctx, id, req.ChannelID, msg.ID, *req.Components)
		}
		if e == nil {
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
		channel, e = DeleteAppMessage(ctx, a, req.ChannelID, req.MessageID)
		if e == nil && m.Effects.Deleted != nil {
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
		e = ErrInvalidInput
	}
	return pluginResponse(result, e)
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
	case errors.Is(err, storage.ErrAppStorageQuota):
		code = "quota_exceeded"
	}
	return json.Marshal(map[string]any{"ok": false, "error": map[string]string{"code": code}})
}
