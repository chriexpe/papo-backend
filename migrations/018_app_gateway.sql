-- +goose Up
-- A época persiste entre processos e reinícios; a confirmação sempre a verifica.
ALTER TABLE app_delivery_state ADD COLUMN connection_epoch BIGINT NOT NULL DEFAULT 0 CHECK (connection_epoch >= 0);

-- +goose Down
ALTER TABLE app_delivery_state DROP COLUMN connection_epoch;
