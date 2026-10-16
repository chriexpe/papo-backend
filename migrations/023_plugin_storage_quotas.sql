-- +goose Up
ALTER TABLE app_installations ADD COLUMN storage_quota JSONB NOT NULL DEFAULT '{}';
ALTER TABLE app_installations ADD CONSTRAINT app_storage_quota_object CHECK(jsonb_typeof(storage_quota)='object');
ALTER TABLE app_storage DROP CONSTRAINT app_storage_value_check;
ALTER TABLE app_storage ADD CONSTRAINT app_storage_value_check CHECK(octet_length(value::text)<=65536);
-- +goose Down
ALTER TABLE app_storage DROP CONSTRAINT app_storage_value_check;
-- Preserva valores maiores existentes; novas escritas voltam ao teto legado.
ALTER TABLE app_storage ADD CONSTRAINT app_storage_value_check CHECK(octet_length(value::text)<=16384) NOT VALID;
ALTER TABLE app_installations DROP CONSTRAINT app_storage_quota_object;
ALTER TABLE app_installations DROP COLUMN storage_quota;
