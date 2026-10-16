-- +goose Up
-- Migration do fork ainda não integrado: substitui o antigo modelo de abas.
ALTER TABLE channels DROP CONSTRAINT channels_type_check;
ALTER TABLE channels ADD CONSTRAINT channels_type_check CHECK (type IN ('text','category','voice','dm','app'));
CREATE TABLE app_channels (
 channel_id UUID PRIMARY KEY REFERENCES channels(id) ON DELETE CASCADE,
 app_installation_id UUID NOT NULL REFERENCES app_installations(id) ON DELETE CASCADE,
 surface_id TEXT NOT NULL,
 source TEXT NOT NULL CHECK (source IN ('bundled','remote')),
 entry TEXT NOT NULL,
 UNIQUE(app_installation_id,surface_id)
);
ALTER TABLE embeds DROP CONSTRAINT embeds_source_type_check;
ALTER TABLE embeds ADD CONSTRAINT embeds_source_type_check CHECK (source_type IN ('link','custom','app'));
ALTER TABLE embeds ADD COLUMN app_installation_id UUID REFERENCES app_installations(id),
 ADD COLUMN app_surface_id TEXT, ADD COLUMN frame_width INTEGER, ADD COLUMN frame_height INTEGER;
ALTER TABLE embeds ADD CONSTRAINT embeds_app_surface_check CHECK (
 (source_type='app' AND app_installation_id IS NOT NULL AND app_surface_id IS NOT NULL
  AND frame_width IS NOT NULL AND frame_height IS NOT NULL AND frame_width BETWEEN 1 AND 1920 AND frame_height BETWEEN 1 AND 1080)
 OR (source_type<>'app' AND app_installation_id IS NULL AND app_surface_id IS NULL));

-- +goose Down
DELETE FROM channels WHERE type='app';
DROP TABLE app_channels;
DELETE FROM embeds WHERE source_type='app';
ALTER TABLE embeds DROP CONSTRAINT embeds_app_surface_check;
ALTER TABLE embeds DROP COLUMN app_installation_id, DROP COLUMN app_surface_id,
 DROP COLUMN frame_width, DROP COLUMN frame_height;
ALTER TABLE embeds DROP CONSTRAINT embeds_source_type_check;
ALTER TABLE embeds ADD CONSTRAINT embeds_source_type_check CHECK (source_type IN ('link','custom'));
ALTER TABLE channels DROP CONSTRAINT channels_type_check;
ALTER TABLE channels ADD CONSTRAINT channels_type_check CHECK (type IN ('text','category','voice','dm'));
