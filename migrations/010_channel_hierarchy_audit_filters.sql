-- +goose Up
ALTER TABLE channels
    ADD COLUMN parent_id UUID REFERENCES channels(id) ON DELETE SET NULL,
    ADD CONSTRAINT channels_parent_not_self CHECK (parent_id IS NULL OR parent_id <> id);

-- Before parent_id, clients inferred that a category owned following channels
-- until the next category. Preserve that layout when making membership explicit.
UPDATE channels AS child
SET parent_id = (
    SELECT category.id
    FROM channels AS category
    WHERE category.type = 'category'
      AND category.position < child.position
    ORDER BY category.position DESC, category.created_at DESC, category.id DESC
    LIMIT 1
)
WHERE child.type <> 'category';

CREATE INDEX IF NOT EXISTS channels_parent_id_idx
    ON channels (parent_id);

CREATE INDEX IF NOT EXISTS idx_audit_logs_target_user_created
    ON audit_logs (target_user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_audit_logs_channel_created
    ON audit_logs ((metadata->>'channel_id'), created_at DESC)
    WHERE metadata ? 'channel_id';

-- +goose Down
DROP INDEX IF EXISTS idx_audit_logs_channel_created;
DROP INDEX IF EXISTS idx_audit_logs_target_user_created;
DROP INDEX IF EXISTS channels_parent_id_idx;

ALTER TABLE channels
    DROP CONSTRAINT IF EXISTS channels_parent_not_self,
    DROP COLUMN IF EXISTS parent_id;
