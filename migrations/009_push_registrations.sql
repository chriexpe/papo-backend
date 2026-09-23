-- +goose Up
CREATE TABLE IF NOT EXISTS push_registrations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    connection_id UUID NOT NULL REFERENCES user_connections(id) ON DELETE CASCADE,
    installation_id UUID NOT NULL,
    provider TEXT NOT NULL,
    address TEXT NOT NULL,
    auth_token TEXT,
    platform TEXT NOT NULL,
    app_version TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, installation_id, provider),
    UNIQUE (provider, address)
);

CREATE INDEX IF NOT EXISTS idx_push_registrations_user
    ON push_registrations (user_id);
CREATE INDEX IF NOT EXISTS idx_push_registrations_connection
    ON push_registrations (connection_id);

-- +goose Down
DROP INDEX IF EXISTS idx_push_registrations_connection;
DROP INDEX IF EXISTS idx_push_registrations_user;
DROP TABLE IF EXISTS push_registrations;
