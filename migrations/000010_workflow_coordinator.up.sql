CREATE TABLE workflow_schedule_reservations (
    id text PRIMARY KEY,
    run_id text NOT NULL,
    step_id text NOT NULL,
    tenant_id text NOT NULL,
    environment text NOT NULL,
    step_payload jsonb NOT NULL,
    owner_token text NOT NULL,
    fence bigint NOT NULL CHECK (fence > 0),
    status text NOT NULL CHECK (status IN ('RESERVED', 'ADMITTED', 'UNKNOWN', 'SUCCEEDED', 'FAILED')),
    lease_expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (run_id, step_id)
);

CREATE INDEX workflow_schedule_reservations_active_idx
    ON workflow_schedule_reservations (tenant_id, environment, status)
    WHERE status IN ('RESERVED', 'ADMITTED', 'UNKNOWN');

CREATE TABLE workflow_schedule_counters (
    dimension text NOT NULL,
    scope_key text NOT NULL,
    limit_value integer NOT NULL CHECK (limit_value > 0),
    active_count integer NOT NULL DEFAULT 0 CHECK (active_count >= 0),
    PRIMARY KEY (dimension, scope_key)
);

CREATE TABLE workflow_schedule_scopes (
    reservation_id text NOT NULL REFERENCES workflow_schedule_reservations(id),
    dimension text NOT NULL,
    scope_key text NOT NULL,
    active boolean NOT NULL DEFAULT true,
    PRIMARY KEY (reservation_id, dimension, scope_key),
    FOREIGN KEY (dimension, scope_key)
        REFERENCES workflow_schedule_counters(dimension, scope_key)
);

CREATE INDEX workflow_schedule_scopes_active_idx
    ON workflow_schedule_scopes (dimension, scope_key)
    WHERE active;

CREATE TABLE workflow_schedule_admissions (
    reservation_id text NOT NULL REFERENCES workflow_schedule_reservations(id),
    operation_key text NOT NULL,
    fence bigint NOT NULL CHECK (fence > 0),
    status text NOT NULL CHECK (status IN ('ADMITTED', 'UNKNOWN', 'SUCCEEDED', 'FAILED')),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (reservation_id, operation_key)
);

CREATE TABLE workflow_schedule_outcomes (
    reservation_id text PRIMARY KEY REFERENCES workflow_schedule_reservations(id),
    first_outcome text NOT NULL CHECK (first_outcome IN ('UNKNOWN', 'SUCCEEDED', 'FAILED')),
    final_outcome text CHECK (final_outcome IN ('SUCCEEDED', 'FAILED')),
    recorded_at timestamptz NOT NULL,
    resolved_at timestamptz
);

CREATE TABLE workflow_circuit_breakers (
    tenant_id text NOT NULL,
    environment text NOT NULL,
    policy jsonb NOT NULL,
    samples jsonb NOT NULL DEFAULT '[]'::jsonb,
    is_open boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, environment)
);
