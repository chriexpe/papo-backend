package models

import (
	"encoding/json"
	"time"
)

// AppCapabilities limita as operações da instalação aos canais autorizados.
type AppCapabilities struct {
	SendInvites    bool `json:"send_invites"`
	PrivateStorage bool `json:"private_storage"`
	SendMessages   bool `json:"send_messages"`
	ReadMessages   bool `json:"read_messages"`
	PostAppEmbeds  bool `json:"post_app_embeds"`
}

// AppInstallation representa uma identidade de máquina pertencente ao servidor.
// CreatedBy é histórico e propriedade de armazenamento, nunca autoria ou autoridade.
type AppInstallation struct {
	StorageQuota          AppStorageQuota         `json:"storage_quota"`
	StorageQuotaOverrides AppStorageQuotaOverride `json:"storage_quota_overrides"`
	LifecyclePending      bool                    `json:"transition_pending,omitempty"`
	PluginDigest          *string                 `json:"plugin_digest,omitempty"`
	PluginVersion         string                  `json:"version,omitempty"`
	PluginSettings        json.RawMessage         `json:"settings,omitempty"`
	ExplicitScopes        json.RawMessage         `json:"explicit_scopes,omitempty"`
	ApprovedOrigins       json.RawMessage         `json:"approved_origins,omitempty"`
	ApprovedPermissions   json.RawMessage         `json:"approved_permissions,omitempty"`
	PluginSurfaces        json.RawMessage         `json:"-"`
	PluginState           string                  `json:"state,omitempty"`
	MaxMemoryMB           int                     `json:"max_memory_mb,omitempty"`
	PluginRevision        int64                   `json:"revision,omitempty"`
	ID                    string                  `json:"id"`
	AppKey                string                  `json:"app_key"`
	Kind                  string                  `json:"kind"`
	Name                  string                  `json:"name"`
	DisplayName           *string                 `json:"display_name,omitempty"`
	AvatarMedia           *string                 `json:"-"`
	Capabilities          AppCapabilities         `json:"capabilities"`
	CreatedBy             string                  `json:"created_by"`
	CreatedAt             time.Time               `json:"created_at"`
	RevokedAt             *time.Time              `json:"revoked_at,omitempty"`
}

// AppList contém uma página de instalações ativas.
type AppList struct {
	Apps    []AppInstallation `json:"apps"`
	HasMore bool              `json:"has_more"`
}
