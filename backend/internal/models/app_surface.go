package models

// AppSurfaceDeclaration é a declaração validada de uma superfície do plugin.
type AppSurfaceDeclaration struct {
	ID          string         `json:"id"`
	Kind        string         `json:"kind"`
	DefaultName string         `json:"default_name,omitempty"`
	Source      string         `json:"source"`
	Entry       string         `json:"entry"`
	Size        AppSurfaceSize `json:"size,omitzero"`
	Load        string         `json:"load,omitempty"`
	Display     string         `json:"display,omitempty"`
	Poster      string         `json:"poster,omitempty"`
}

// AppChannel é a superfície que ocupa o canal inteiro, sem conversa.
type AppChannel struct {
	AppInstallationID string `json:"app_installation_id"`
	SurfaceID         string `json:"surface_id"`
	Source            string `json:"source,omitempty"`
	Entry             string `json:"entry,omitempty"`
	AvatarURL         string `json:"avatar_url"`
	Available         bool   `json:"available"`
}

// AppChannelChange contém snapshots para publicar eventos somente depois do commit.
// Before preserva a ACL mesmo quando o canal já não existe.
type AppChannelChange struct {
	Before *Channel
	After  *Channel
}

// AppSurfaceSize é o tamanho autoral do frame ou do card que abre um overlay.
type AppSurfaceSize struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// AppSurfaceReference identifica a superfície sem repetir identidade visual do remetente.
type AppSurfaceReference struct {
	AppInstallationID string `json:"app_installation_id"`
	ID                string `json:"id"`
}
