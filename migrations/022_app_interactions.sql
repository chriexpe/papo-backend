-- +goose Up
ALTER TABLE messages ADD COLUMN components JSONB NOT NULL DEFAULT '[]';
ALTER TABLE app_installations ADD COLUMN ui_generation BIGINT NOT NULL DEFAULT 0;
ALTER TABLE notifications ALTER COLUMN message_id DROP NOT NULL;
ALTER TABLE notifications ADD COLUMN kind TEXT NOT NULL DEFAULT 'message' CHECK(kind IN ('message','app_invite')),
 ADD COLUMN app_installation_id UUID REFERENCES app_installations(id) ON DELETE CASCADE,
 ADD COLUMN payload JSONB NOT NULL DEFAULT '{}', ADD COLUMN expires_at TIMESTAMPTZ,
 ADD COLUMN invite_status TEXT CHECK(invite_status IN ('pending','accepted','declined'));
ALTER TABLE notifications ADD CONSTRAINT notifications_kind_fields CHECK (
 (kind='message' AND message_id IS NOT NULL AND invite_status IS NULL) OR
 (kind='app_invite' AND message_id IS NULL AND app_installation_id IS NOT NULL AND expires_at IS NOT NULL AND invite_status IS NOT NULL));
CREATE INDEX notifications_invite_expiry ON notifications(expires_at) WHERE kind='app_invite';
CREATE TABLE app_user_settings (
 user_id UUID REFERENCES users(id) ON DELETE CASCADE,
 installation_id UUID REFERENCES app_installations(id) ON DELETE CASCADE,
 invites_muted BOOLEAN NOT NULL DEFAULT false, updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(user_id,installation_id));
CREATE TABLE app_ui_tokens (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(), token_hash TEXT NOT NULL UNIQUE,
 kind TEXT NOT NULL CHECK(kind IN ('ticket','session')),
 user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 installation_id UUID NOT NULL REFERENCES app_installations(id) ON DELETE CASCADE,
 channel_id UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
 surface_id TEXT NOT NULL, message_id UUID REFERENCES messages(id) ON DELETE CASCADE,
 invite_id UUID REFERENCES notifications(id) ON DELETE CASCADE,
 generation BIGINT NOT NULL, digest TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), expires_at TIMESTAMPTZ NOT NULL,
 absolute_expires_at TIMESTAMPTZ NOT NULL, consumed BOOLEAN NOT NULL DEFAULT false);
CREATE INDEX app_ui_tokens_expiry ON app_ui_tokens(expires_at);
CREATE INDEX app_ui_tokens_installation ON app_ui_tokens(installation_id,user_id);
CREATE TABLE app_rate_limits (key TEXT PRIMARY KEY, window_at TIMESTAMPTZ NOT NULL, count INT NOT NULL);

-- Mudanças de ACL revogam tokens mesmo se o acesso for concedido novamente antes do próximo RPC.
-- +goose StatementBegin
CREATE FUNCTION revoke_app_ui_acl() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 DELETE FROM app_ui_tokens;
 RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER app_ui_channel_acl AFTER UPDATE OF permissions ON channels FOR EACH STATEMENT EXECUTE FUNCTION revoke_app_ui_acl();
CREATE TRIGGER app_ui_role_acl AFTER UPDATE OF permissions ON roles FOR EACH STATEMENT EXECUTE FUNCTION revoke_app_ui_acl();
CREATE TRIGGER app_ui_membership_acl AFTER INSERT OR UPDATE OR DELETE ON user_roles FOR EACH STATEMENT EXECUTE FUNCTION revoke_app_ui_acl();
CREATE TRIGGER app_ui_ban_acl AFTER UPDATE OF banned ON users FOR EACH STATEMENT EXECUTE FUNCTION revoke_app_ui_acl();

-- +goose Down
DROP TRIGGER app_ui_channel_acl ON channels;
DROP TRIGGER app_ui_role_acl ON roles;
DROP TRIGGER app_ui_membership_acl ON user_roles;
DROP TRIGGER app_ui_ban_acl ON users;
DROP FUNCTION revoke_app_ui_acl();
DROP TABLE app_rate_limits;
DROP TABLE app_ui_tokens;
DROP TABLE app_user_settings;
DELETE FROM push_outbox WHERE notification_id IN (SELECT id FROM notifications WHERE kind='app_invite');
DELETE FROM notifications WHERE kind='app_invite';
ALTER TABLE notifications DROP CONSTRAINT notifications_kind_fields;
DROP INDEX notifications_invite_expiry;
ALTER TABLE notifications DROP COLUMN kind, DROP COLUMN app_installation_id, DROP COLUMN payload, DROP COLUMN expires_at, DROP COLUMN invite_status;
ALTER TABLE notifications ALTER COLUMN message_id SET NOT NULL;
ALTER TABLE app_installations DROP COLUMN ui_generation;
ALTER TABLE messages DROP COLUMN components;
