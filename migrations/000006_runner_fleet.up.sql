CREATE TABLE runner_fleet (
    tenant_id text NOT NULL,
    runner_id text NOT NULL,
    runner_group text NOT NULL,
    status text NOT NULL CHECK (status IN ('READY', 'DRAINING', 'FROZEN')),
    capacity integer NOT NULL CHECK (capacity >= 0),
    reported_capacity integer NOT NULL CHECK (reported_capacity >= 0),
    in_flight integer NOT NULL CHECK (in_flight >= 0 AND in_flight <= reported_capacity),
    last_seen timestamptz NOT NULL,
    state_version bigint NOT NULL CHECK (state_version > 0),
    controlled_by text,
    controlled_at timestamptz,
    frozen_by text,
    frozen_at timestamptz,
    PRIMARY KEY (tenant_id, runner_id)
);

CREATE INDEX runner_fleet_group_status_idx
    ON runner_fleet (tenant_id, runner_group, status, last_seen DESC);
