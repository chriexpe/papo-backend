-- +goose Up
ALTER TABLE app_installations ADD COLUMN artifact_sha TEXT REFERENCES media(sha_hash);
ALTER TABLE app_installations ADD COLUMN artifact_digest TEXT CHECK (artifact_digest ~ '^[0-9a-f]{64}$');
ALTER TABLE app_installations ADD CONSTRAINT wasm_artifact_kind CHECK
    ((artifact_sha IS NULL AND artifact_digest IS NULL) OR
     (kind='wasm' AND artifact_sha IS NOT NULL AND artifact_digest IS NOT NULL));
CREATE TABLE app_storage (
    installation UUID NOT NULL REFERENCES app_installations(id) ON DELETE CASCADE,
    key TEXT NOT NULL CHECK (octet_length(key) BETWEEN 1 AND 200),
    value JSONB NOT NULL CHECK (octet_length(value::text) <= 16384),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    PRIMARY KEY (installation, key)
);

-- +goose Down
DROP TABLE app_storage;
ALTER TABLE app_installations DROP CONSTRAINT wasm_artifact_kind;
ALTER TABLE app_installations DROP COLUMN artifact_digest;
ALTER TABLE app_installations DROP COLUMN artifact_sha;
