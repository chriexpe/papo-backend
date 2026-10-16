package models

import "time"

// AppCapabilities limita as operações da instalação aos canais autorizados.
type AppCapabilities struct {
	SendMessages  bool `json:"send_messages"`
	ReadMessages  bool `json:"read_messages"`
	PostAppEmbeds bool `json:"post_app_embeds"`
}

// AppInstallation representa uma identidade de máquina pertencente ao servidor.
// CreatedBy é histórico e propriedade de armazenamento, nunca autoria ou autoridade.
type AppInstallation struct {
	ID           string          `json:"id"`
	AppKey       string          `json:"app_key"`
	Kind         string          `json:"kind"`
	Name         string          `json:"name"`
	DisplayName  *string         `json:"display_name,omitempty"`
	AvatarMedia  *string         `json:"-"`
	Capabilities AppCapabilities `json:"capabilities"`
	CreatedBy    string          `json:"created_by"`
	CreatedAt    time.Time       `json:"created_at"`
	RevokedAt    *time.Time      `json:"revoked_at,omitempty"`
}

// AppList contém uma página de instalações ativas.
type AppList struct {
	Apps    []AppInstallation `json:"apps"`
	HasMore bool              `json:"has_more"`
}
