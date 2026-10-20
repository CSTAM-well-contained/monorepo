-- +goose Up
-- Manual failover: cpd asks this gateway to step down (the agent writes 1 into keepalived's
-- force-backup file, the instance goes to FAULT and the peer takes the VIP).
ALTER TABLE gateways ADD COLUMN force_backup boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE gateways DROP COLUMN force_backup;
