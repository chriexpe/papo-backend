-- +goose Up
CREATE TABLE channel_app_views (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    app_installation_id UUID NOT NULL REFERENCES app_installations(id) ON DELETE CASCADE,
    channel_id UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    url TEXT NOT NULL CHECK (char_length(url) <= 2048),
    position INTEGER NOT NULL DEFAULT 0 CHECK (position >= 0),
    created_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (app_installation_id, channel_id)
        REFERENCES app_channel_scopes(app_installation_id, channel_id) ON DELETE CASCADE
);
CREATE INDEX idx_channel_app_views_channel ON channel_app_views(channel_id, position, created_at, id);

-- +goose Down
DROP TABLE channel_app_views;
