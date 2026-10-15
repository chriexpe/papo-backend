package models

import "time"

// ChannelAppView descreve conteúdo externo passivo; nunca contém credenciais Papo.
type ChannelAppView struct {
	ID                string    `json:"id"`
	AppInstallationID string    `json:"app_installation_id"`
	ChannelID         string    `json:"channel_id"`
	URL               string    `json:"url"`
	Position          int       `json:"position"`
	CreatedBy         string    `json:"created_by"`
	CreatedAt         time.Time `json:"created_at"`
}
