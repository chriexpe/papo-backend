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
 ADD COLUMN app_surface_id TEXT, ADD COLUMN app_width INTEGER, ADD COLUMN app_height INTEGER,
 ADD COLUMN app_load TEXT CHECK (app_load IN ('click','visible')),
 ADD COLUMN app_display TEXT CHECK (app_display IN ('inline','overlay')),
 ADD COLUMN app_poster TEXT;
ALTER TABLE embeds ADD CONSTRAINT embeds_app_surface_check CHECK (
 (source_type='app' AND app_installation_id IS NOT NULL AND app_surface_id IS NOT NULL
  AND app_width IS NOT NULL AND app_height IS NOT NULL AND app_width > 0 AND app_height > 0
  AND app_load IS NOT NULL AND app_display IS NOT NULL
  AND (app_display <> 'overlay' OR app_load='click'))
 OR (source_type<>'app' AND app_installation_id IS NULL AND app_surface_id IS NULL
  AND app_width IS NULL AND app_height IS NULL AND app_load IS NULL AND app_display IS NULL AND app_poster IS NULL));

-- +goose Down
DELETE FROM channels WHERE type='app';
DROP TABLE app_channels;
DELETE FROM embeds WHERE source_type='app';
ALTER TABLE embeds DROP CONSTRAINT embeds_app_surface_check;
ALTER TABLE embeds DROP COLUMN app_installation_id, DROP COLUMN app_surface_id,
 DROP COLUMN app_width, DROP COLUMN app_height, DROP COLUMN app_load, DROP COLUMN app_display, DROP COLUMN app_poster;
ALTER TABLE embeds DROP CONSTRAINT embeds_source_type_check;
ALTER TABLE embeds ADD CONSTRAINT embeds_source_type_check CHECK (source_type IN ('link','custom'));
ALTER TABLE channels DROP CONSTRAINT channels_type_check;
ALTER TABLE channels ADD CONSTRAINT channels_type_check CHECK (type IN ('text','category','voice','dm'));
