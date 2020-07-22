-- +goose Up
-- SQL in this section is executed when the migration is applied.
ALTER TABLE smtp ADD COLUMN proxy_address varchar(255);
ALTER TABLE smtp ADD COLUMN proxy_username varchar(255);
ALTER TABLE smtp ADD COLUMN proxy_password varchar(255);

-- +goose Down
-- SQL in this section is executed when the migration is rolled back.
