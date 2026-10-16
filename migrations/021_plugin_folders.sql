-- +goose Up
ALTER TABLE app_installations DROP CONSTRAINT wasm_artifact_kind;
ALTER TABLE app_installations DROP CONSTRAINT app_installations_kind_check;
-- A CHECK anônima de 016 inclui tipos legados e deve ser trocada antes do UPDATE.
-- +goose StatementBegin
DO $$ DECLARE r record; BEGIN
 FOR r IN SELECT conname FROM pg_constraint WHERE conrelid='app_installations'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%secret_hash%'
 LOOP EXECUTE format('ALTER TABLE app_installations DROP CONSTRAINT %I',r.conname); END LOOP;
END $$;
-- +goose StatementEnd
UPDATE app_installations SET kind='plugin' WHERE kind IN ('view','wasm');
UPDATE app_installations SET capabilities='{}'::jsonb WHERE kind='plugin';
DELETE FROM app_delivery_state WHERE installation_id IN (SELECT id FROM app_installations WHERE kind='plugin');
ALTER TABLE app_installations ADD CONSTRAINT app_installations_kind_check CHECK(kind IN ('remote','plugin'));
ALTER TABLE app_installations ADD CONSTRAINT app_installations_secret_kind CHECK
 ((kind='remote' AND secret_hash IS NOT NULL) OR (kind='plugin' AND secret_hash IS NULL));
ALTER TABLE app_installations ADD COLUMN plugin_digest TEXT;
ALTER TABLE app_installations ADD COLUMN version TEXT NOT NULL DEFAULT '';
ALTER TABLE app_installations ADD COLUMN settings JSONB NOT NULL DEFAULT '{}';
ALTER TABLE app_installations ADD COLUMN explicit_scopes JSONB NOT NULL DEFAULT '[]';
ALTER TABLE app_installations ADD COLUMN approved_origins JSONB NOT NULL DEFAULT '[]';
ALTER TABLE app_installations ADD COLUMN approved_permissions JSONB NOT NULL DEFAULT '[]';
ALTER TABLE app_installations ADD COLUMN plugin_surfaces JSONB NOT NULL DEFAULT '[]';
ALTER TABLE app_installations ADD COLUMN state TEXT NOT NULL DEFAULT 'disabled' CHECK(state IN ('enabled','disabled','quarantined','removed'));
ALTER TABLE app_installations ADD COLUMN max_memory_mb INTEGER NOT NULL DEFAULT 16 CHECK(max_memory_mb BETWEEN 1 AND 256);
ALTER TABLE app_installations ADD COLUMN lifecycle_pending BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE app_installations ADD COLUMN plugin_revision BIGINT NOT NULL DEFAULT 0;
CREATE INDEX idx_app_installations_plugin_key ON app_installations(app_key) WHERE kind='plugin';

-- +goose Down
DROP INDEX idx_app_installations_plugin_key;
ALTER TABLE app_installations DROP COLUMN lifecycle_pending,DROP COLUMN plugin_revision, DROP COLUMN max_memory_mb, DROP COLUMN state,
 DROP COLUMN explicit_scopes,DROP COLUMN plugin_surfaces,DROP COLUMN approved_permissions,DROP COLUMN approved_origins,DROP COLUMN settings,DROP COLUMN version,DROP COLUMN plugin_digest;
ALTER TABLE app_installations DROP CONSTRAINT app_installations_secret_kind;
ALTER TABLE app_installations DROP CONSTRAINT app_installations_kind_check;
UPDATE app_installations SET kind='wasm' WHERE kind='plugin';
ALTER TABLE app_installations ADD CONSTRAINT app_installations_kind_check CHECK(kind IN ('remote','view','wasm'));
ALTER TABLE app_installations ADD CONSTRAINT app_installations_check CHECK
 ((kind='remote' AND secret_hash IS NOT NULL) OR (kind IN ('view','wasm') AND secret_hash IS NULL));
ALTER TABLE app_installations ADD CONSTRAINT wasm_artifact_kind CHECK
 ((artifact_sha IS NULL AND artifact_digest IS NULL) OR (kind='wasm' AND artifact_sha IS NOT NULL AND artifact_digest IS NOT NULL));
