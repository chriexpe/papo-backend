-- +goose Up
-- Outbox de eventos para apps remotos: cada fato é gravado na mesma transação
-- da mutação (mensagem, reação, exclusão de canal) e somente quando há ao
-- menos um app leitor do canal. channel_id/message_id não têm FK de propósito:
-- os tombstones de exclusão precisam sobreviver à remoção da linha original.
-- recipients é preenchido apenas nos tombstones, capturando os destinatários
-- antes da cascata de escopos; os demais eventos revalidam a leitura na entrega.
CREATE TABLE app_event_outbox (
    event_id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type    TEXT NOT NULL,
    channel_id    UUID NOT NULL,
    message_id    UUID,
    payload       JSONB NOT NULL,
    recipients    UUID[],
    created_at    TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    dispatched_at TIMESTAMPTZ,
    CONSTRAINT app_event_outbox_event_type_check CHECK (event_type IN (
        'message.created', 'message.updated', 'message.deleted',
        'reaction.added', 'reaction.removed', 'channel.deleted')),
    CONSTRAINT app_event_outbox_recipients_check CHECK (
        (event_type IN ('message.deleted', 'channel.deleted')) = (recipients IS NOT NULL))
);

CREATE INDEX idx_app_events_claim ON app_event_outbox (created_at, event_id)
    WHERE dispatched_at IS NULL;
CREATE INDEX idx_app_events_prune ON app_event_outbox (created_at, event_id);
CREATE INDEX idx_app_events_redact ON app_event_outbox (message_id)
    WHERE event_type IN ('message.created', 'message.updated');
CREATE INDEX idx_app_events_channel ON app_event_outbox (channel_id);

-- Estado de entrega por instalação: next_seq é a última sequência atribuída,
-- acked_seq o último ack durável e replay_floor o maior prefixo já podado
-- (abaixo dele o app precisa reconciliar por REST: resync_required).
CREATE TABLE app_delivery_state (
    installation_id UUID PRIMARY KEY REFERENCES app_installations(id) ON DELETE CASCADE,
    next_seq        BIGINT NOT NULL DEFAULT 0 CHECK (next_seq >= 0),
    acked_seq       BIGINT NOT NULL DEFAULT 0 CHECK (acked_seq >= 0 AND acked_seq <= next_seq),
    replay_floor    BIGINT NOT NULL DEFAULT 0 CHECK (replay_floor >= 0 AND replay_floor <= next_seq)
);

-- Entregas pendentes: a sequência é atribuída pelo dispatcher, por
-- instalação, e removida no ack ou na poda.
CREATE TABLE app_event_deliveries (
    installation_id UUID NOT NULL REFERENCES app_delivery_state(installation_id) ON DELETE CASCADE,
    delivery_seq    BIGINT NOT NULL CHECK (delivery_seq > 0),
    event_id        UUID NOT NULL REFERENCES app_event_outbox(event_id) ON DELETE CASCADE,
    PRIMARY KEY (installation_id, delivery_seq),
    UNIQUE (installation_id, event_id)
);

CREATE INDEX idx_app_deliveries_event ON app_event_deliveries (event_id);

-- +goose Down
DROP TABLE app_event_deliveries;
DROP TABLE app_delivery_state;
DROP TABLE app_event_outbox;
