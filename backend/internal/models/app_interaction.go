package models

import (
	"encoding/json"
	"time"
)

// AppButton é um controle nativo; opens dispensa a execução do plugin.
type AppButton struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Style    string `json:"style"`
	Disabled bool   `json:"disabled"`
	Opens    string `json:"opens,omitempty"`
}

// AppUserAction aparece apenas nos canais próprios da instalação.
type AppUserAction struct {
	ID    string `toml:"id" json:"id"`
	Label string `toml:"label" json:"label"`
	Icon  string `toml:"icon" json:"icon,omitempty"`
}

// AppInvite contém somente dados verificados pelo servidor.
type AppInvite struct {
	SurfaceID      string          `json:"surface_id,omitempty"`
	ID             string          `json:"id"`
	UserID         string          `json:"-"`
	InstallationID string          `json:"app_installation_id"`
	InviterID      string          `json:"inviter_id"`
	ChannelID      string          `json:"channel_id"`
	Text           string          `json:"text"`
	Data           json.RawMessage `json:"data"`
	Status         string          `json:"status"`
	ExpiresAt      time.Time       `json:"expires_at"`
}

// AppUIToken guarda o hash, nunca o segredo entregue à página.
type AppUIToken struct {
	ID, Hash, Kind, UserID, InstallationID, ChannelID, SurfaceID, Digest string
	MessageID, InviteID                                                  *string
	Generation                                                           int64
	ExpiresAt, AbsoluteExpiresAt                                         time.Time
}
