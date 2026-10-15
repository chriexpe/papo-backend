-- +goose Up
-- Instalações pertencem ao servidor; created_by registra o instalador e o dono da mídia.
CREATE TABLE app_installations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    app_key TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('remote', 'view', 'wasm')),
    name TEXT NOT NULL,
    display_name TEXT,
    avatar_media TEXT REFERENCES media(sha_hash),
    capabilities JSONB NOT NULL DEFAULT '{}'::jsonb,
    secret_hash TEXT,
    created_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ,
    CHECK ((kind = 'remote' AND secret_hash IS NOT NULL) OR
           (kind IN ('view', 'wasm') AND secret_hash IS NULL))
);
CREATE INDEX idx_app_installations_secret_hash ON app_installations(secret_hash)
    WHERE secret_hash IS NOT NULL;
CREATE TABLE app_channel_scopes (
    app_installation_id UUID NOT NULL REFERENCES app_installations(id) ON DELETE CASCADE,
    channel_id UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    created_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (app_installation_id, channel_id)
);
CREATE INDEX idx_app_channel_scopes_channel ON app_channel_scopes(channel_id);
ALTER TABLE messages ADD COLUMN app_installation_id UUID REFERENCES app_installations(id);
CREATE UNIQUE INDEX idx_messages_app_external_id
    ON messages(app_installation_id, external_message_id)
    WHERE app_installation_id IS NOT NULL AND external_message_id IS NOT NULL;

-- +goose Down
DROP INDEX idx_messages_app_external_id;
ALTER TABLE messages DROP COLUMN app_installation_id;
DROP TABLE app_channel_scopes;
DROP TABLE app_installations;
