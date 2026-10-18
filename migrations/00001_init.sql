-- +goose Up
-- Names reach HAProxy, Neutron and Docker: same rule as CLAUDE.md.
CREATE DOMAIN dns_label AS text CHECK (VALUE ~ '^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$');

CREATE TABLE hackathons (
    id          bigserial PRIMARY KEY,
    name        dns_label   NOT NULL UNIQUE,
    domain      text        NOT NULL CHECK (domain ~ '^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$'),
    state       text        NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'ending', 'ended')),
    lease_ttl   interval    NOT NULL DEFAULT '2 hours' CHECK (lease_ttl >= '10 seconds'),
    created_at  timestamptz NOT NULL DEFAULT now(),
    ended_at    timestamptz
);

-- A team = one sandbox with one private IP, published at <team>.<hackathon domain>.
CREATE TABLE teams (
    id               bigserial PRIMARY KEY,
    hackathon_id     bigint      NOT NULL REFERENCES hackathons (id),
    name             dns_label   NOT NULL,
    state            text        NOT NULL CHECK (state IN ('provisioning', 'ready', 'failed', 'deleting', 'deleted')),
    ip               inet,
    slot             int         CHECK (slot BETWEEN 1 AND 999),
    app_port         int         NOT NULL DEFAULT 80 CHECK (app_port BETWEEN 1 AND 65535),
    image            text        NOT NULL DEFAULT '',
    sandbox_ref      text        NOT NULL DEFAULT '', -- driver id: Nova server + Neutron port, or a container
    lease_expires_at timestamptz NOT NULL,
    error            text        NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
-- live teams: unique name per hackathon, unique slot, unique IP
CREATE UNIQUE INDEX teams_name_live ON teams (hackathon_id, name) WHERE state <> 'deleted';
CREATE UNIQUE INDEX teams_slot_live ON teams (slot) WHERE state <> 'deleted' AND slot IS NOT NULL;
CREATE UNIQUE INDEX teams_ip_live   ON teams (ip)   WHERE state <> 'deleted' AND ip IS NOT NULL;
CREATE INDEX teams_lease ON teams (lease_expires_at) WHERE state IN ('provisioning', 'ready');

-- IPAM: one row per address of the team pool.
--   free -> allocated (lease) -> quarantine (route removed, waiting for every gateway ack) -> free
CREATE TABLE ip_pool (
    ip           inet        PRIMARY KEY,
    state        text        NOT NULL CHECK (state IN ('free', 'allocated', 'quarantine', 'reserved')),
    team_id      bigint      REFERENCES teams (id),
    released_gen bigint,      -- routes generation that no longer contains this IP
    released_at  timestamptz,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CHECK ((state = 'allocated') = (team_id IS NOT NULL))
);
CREATE INDEX ip_pool_free ON ip_pool (ip) WHERE state = 'free';

-- Every change of the published routes bumps this counter (single row).
CREATE TABLE routes_generation (
    id  boolean PRIMARY KEY DEFAULT true CHECK (id),
    gen bigint  NOT NULL
);
INSERT INTO routes_generation VALUES (true, 0);

-- What each gateway last confirmed.
CREATE TABLE gateways (
    name           dns_label   PRIMARY KEY,
    acked_gen      bigint      NOT NULL DEFAULT 0,
    config_version text        NOT NULL DEFAULT '',
    vrrp_state     text        NOT NULL DEFAULT '',
    vrrp_since     timestamptz,
    last_seen      timestamptz NOT NULL DEFAULT now()
);

-- Append-only audit and event log (dashboard, benchmark report).
CREATE TABLE events (
    id      bigserial   PRIMARY KEY,
    at      timestamptz NOT NULL DEFAULT now(),
    kind    text        NOT NULL,
    team_id bigint,
    detail  jsonb       NOT NULL DEFAULT '{}'
);

-- +goose Down
DROP TABLE events, gateways, routes_generation, ip_pool, teams, hackathons;
DROP DOMAIN dns_label;
