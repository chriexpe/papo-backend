package models

import "time"

// PushRegistration is internal transport credential state. Address/AuthToken
// must never be serialized to API clients or written to logs.
type PushRegistration struct {
	ID             string
	UserID         string
	ConnectionID   string
	InstallationID string
	Provider       string
	Address        string
	AuthToken      *string
	Platform       string
	AppVersion     *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
