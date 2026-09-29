-- +goose Up
ALTER TABLE telegram_channels ADD COLUMN source_type text NOT NULL DEFAULT 'auto'
  CHECK (source_type IN ('auto','web_channel','mtproto_group'));

ALTER TABLE posts ADD COLUMN photo_data bytea;
ALTER TABLE posts ADD COLUMN photo_mime text;

ALTER TABLE listings DROP CONSTRAINT listings_extraction_status_check;
ALTER TABLE listings ADD CONSTRAINT listings_extraction_status_check
  CHECK(extraction_status IN ('success','unparsed','ignored_non_listing'));

CREATE TABLE telegram_account (
  id smallint PRIMARY KEY DEFAULT 1 CHECK (id=1),
  api_id integer NOT NULL DEFAULT 2040,
  api_hash text NOT NULL DEFAULT 'b18441a1ff607e10a98964a5b8c7a4d0',
  phone text NOT NULL DEFAULT '',
  session_data bytea,
  status text NOT NULL DEFAULT 'disconnected',
  last_error text NOT NULL DEFAULT '',
  connected_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO telegram_account(id) VALUES(1) ON CONFLICT DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS telegram_account;
ALTER TABLE posts DROP COLUMN IF EXISTS photo_mime;
ALTER TABLE posts DROP COLUMN IF EXISTS photo_data;
ALTER TABLE telegram_channels DROP COLUMN IF EXISTS source_type;
