-- +goose Up
-- HAProxy settings that cpd owns (changing them means a reload on every gateway). The newest
-- row is the desired config, published to the agents as version 'c<id>'. A rollback inserts a
-- copy of an older row (copied_from): history is never rewritten, so a gateway that was offline
-- simply converges to the newest version.
CREATE TABLE config_versions (
    id          bigserial   PRIMARY KEY,
    rate_limit  int         NOT NULL CHECK (rate_limit BETWEEN 1 AND 1000000),
    extra       text[]      NOT NULL DEFAULT '{}',
    reason      text        NOT NULL DEFAULT '',
    copied_from bigint      REFERENCES config_versions (id),
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- What each gateway did with the last config version it tried.
ALTER TABLE gateways
    ADD COLUMN config_tried   text NOT NULL DEFAULT '',
    ADD COLUMN config_outcome text NOT NULL DEFAULT '',
    ADD COLUMN config_error   text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE gateways DROP COLUMN config_tried, DROP COLUMN config_outcome, DROP COLUMN config_error;
DROP TABLE config_versions;
