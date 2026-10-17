-- +goose Up
-- Época da conexão do gateway de apps: cada hello válido a incrementa e todo
-- ack precisa informar a época atual, descartando confirmações de uma conexão
-- já substituída.
ALTER TABLE app_delivery_state
    ADD COLUMN connection_epoch BIGINT NOT NULL DEFAULT 0 CHECK (connection_epoch >= 0);

-- +goose Down
ALTER TABLE app_delivery_state DROP COLUMN connection_epoch;
