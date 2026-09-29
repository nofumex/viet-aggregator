-- +goose Up
ALTER TABLE listings
  ADD COLUMN furnished text,
  ADD COLUMN near_beach boolean,
  ADD COLUMN beach_distance_m integer,
  ADD COLUMN amenities jsonb NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE listings
  DROP COLUMN amenities,
  DROP COLUMN beach_distance_m,
  DROP COLUMN near_beach,
  DROP COLUMN furnished;
