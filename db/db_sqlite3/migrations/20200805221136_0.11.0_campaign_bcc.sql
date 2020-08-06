-- +goose Up
-- SQL in this section is executed when the migration is applied.
ALTER TABLE campaigns ADD COLUMN bcc BOOLEAN;

-- +goose Down
-- SQL in this section is executed when the migration is rolled back.
