-- +goose Up
-- Plugins de pasta (kind = 'plugin'): estado do consentimento, superfícies
-- aprovadas e limites de execução da instalação.
ALTER TABLE app_installations
    ADD COLUMN plugin_digest        TEXT,
    ADD COLUMN version              TEXT NOT NULL DEFAULT '',
    ADD COLUMN settings             JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN explicit_scopes      JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN approved_origins     JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN approved_permissions JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN plugin_surfaces      JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN state                TEXT NOT NULL DEFAULT 'disabled'
        CHECK (state IN ('enabled', 'disabled', 'quarantined', 'removed')),
    ADD COLUMN max_memory_mb        INTEGER NOT NULL DEFAULT 16 CHECK (max_memory_mb BETWEEN 1 AND 256),
    ADD COLUMN lifecycle_pending    BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN plugin_revision      BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ui_generation        BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN storage_quota        JSONB NOT NULL DEFAULT '{}'
        CONSTRAINT app_storage_quota_object CHECK (jsonb_typeof(storage_quota) = 'object');

CREATE INDEX idx_app_installations_plugin_key ON app_installations (app_key) WHERE kind = 'plugin';

-- Armazenamento chave/valor privado de cada instalação.
CREATE TABLE app_storage (
    installation UUID NOT NULL REFERENCES app_installations(id) ON DELETE CASCADE,
    key          TEXT NOT NULL CHECK (octet_length(key) BETWEEN 1 AND 200),
    value        JSONB NOT NULL CONSTRAINT app_storage_value_check CHECK (octet_length(value::text) <= 65536),
    revision     BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    PRIMARY KEY (installation, key)
);

-- Uso acumulado de app_storage por instalação (chaves e bytes de chave + JSONB
-- normalizado), mantido na mesma transação de cada escrita para checar a
-- quota sem somar todas as chaves.
CREATE TABLE app_storage_usage (
    installation UUID PRIMARY KEY REFERENCES app_installations(id) ON DELETE CASCADE,
    keys         INTEGER NOT NULL DEFAULT 0 CHECK (keys >= 0),
    bytes        BIGINT NOT NULL DEFAULT 0 CHECK (bytes >= 0)
);

-- Botões de mensagens de apps (até duas linhas de cinco).
ALTER TABLE messages ADD COLUMN components JSONB NOT NULL DEFAULT '[]';

-- Convites de app reutilizam notifications: message_id passa a ser opcional e
-- kind distingue a notificação de mensagem do convite.
ALTER TABLE notifications ALTER COLUMN message_id DROP NOT NULL;
ALTER TABLE notifications
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'message' CHECK (kind IN ('message', 'app_invite')),
    ADD COLUMN app_installation_id UUID REFERENCES app_installations(id) ON DELETE CASCADE,
    ADD COLUMN payload JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN expires_at TIMESTAMPTZ,
    ADD COLUMN invite_status TEXT CHECK (invite_status IN ('pending', 'accepted', 'declined'));
ALTER TABLE notifications ADD CONSTRAINT notifications_kind_fields CHECK (
    (kind = 'message' AND message_id IS NOT NULL AND invite_status IS NULL) OR
    (kind = 'app_invite' AND message_id IS NULL AND app_installation_id IS NOT NULL
        AND expires_at IS NOT NULL AND invite_status IS NOT NULL));
CREATE INDEX notifications_invite_expiry ON notifications (expires_at) WHERE kind = 'app_invite';

CREATE TABLE app_user_settings (
    user_id         UUID REFERENCES users(id) ON DELETE CASCADE,
    installation_id UUID REFERENCES app_installations(id) ON DELETE CASCADE,
    invites_muted   BOOLEAN NOT NULL DEFAULT false,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, installation_id)
);

-- Tickets e sessões da ponte UI↔plugin; somente o SHA-256 do segredo.
-- generation é comparada com app_installations.ui_generation: disable,
-- quarentena e nova aprovação incrementam a geração e invalidam tudo. Leitura
-- do canal, banimento e estado da instalação são revalidados a cada chamada.
CREATE TABLE app_ui_tokens (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash          TEXT NOT NULL UNIQUE,
    kind                TEXT NOT NULL CHECK (kind IN ('ticket', 'session')),
    user_id             UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    installation_id     UUID NOT NULL REFERENCES app_installations(id) ON DELETE CASCADE,
    channel_id          UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    surface_id          TEXT NOT NULL,
    message_id          UUID REFERENCES messages(id) ON DELETE CASCADE,
    invite_id           UUID REFERENCES notifications(id) ON DELETE CASCADE,
    generation          BIGINT NOT NULL,
    digest              TEXT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at          TIMESTAMPTZ NOT NULL,
    absolute_expires_at TIMESTAMPTZ NOT NULL,
    consumed            BOOLEAN NOT NULL DEFAULT false
);
CREATE INDEX app_ui_tokens_expiry ON app_ui_tokens (expires_at);
CREATE INDEX app_ui_tokens_installation ON app_ui_tokens (installation_id, user_id);

-- +goose Down
DROP TABLE app_ui_tokens;
DROP TABLE app_user_settings;
DELETE FROM push_outbox WHERE notification_id IN (SELECT id FROM notifications WHERE kind = 'app_invite');
DELETE FROM notifications WHERE kind = 'app_invite';
DROP INDEX notifications_invite_expiry;
ALTER TABLE notifications DROP CONSTRAINT notifications_kind_fields;
ALTER TABLE notifications
    DROP COLUMN kind,
    DROP COLUMN app_installation_id,
    DROP COLUMN payload,
    DROP COLUMN expires_at,
    DROP COLUMN invite_status;
ALTER TABLE notifications ALTER COLUMN message_id SET NOT NULL;
ALTER TABLE messages DROP COLUMN components;
DROP TABLE app_storage_usage;
DROP TABLE app_storage;
DROP INDEX idx_app_installations_plugin_key;
ALTER TABLE app_installations
    DROP COLUMN storage_quota,
    DROP COLUMN ui_generation,
    DROP COLUMN plugin_revision,
    DROP COLUMN lifecycle_pending,
    DROP COLUMN max_memory_mb,
    DROP COLUMN state,
    DROP COLUMN plugin_surfaces,
    DROP COLUMN approved_permissions,
    DROP COLUMN approved_origins,
    DROP COLUMN explicit_scopes,
    DROP COLUMN settings,
    DROP COLUMN version,
    DROP COLUMN plugin_digest;
