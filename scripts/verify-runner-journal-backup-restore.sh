#!/bin/sh
set -eu

compose_file=deploy/compose/compose.yaml
restore_db="themisy_restore_verify_$(date +%s)_$$"
tmp_dir=$(mktemp -d)
dump_file="$tmp_dir/runner.dump"
restore_created=0

cleanup() {
	if [ "$restore_created" -eq 1 ]; then
		docker compose -f "$compose_file" exec -T postgres dropdb -U themisy "$restore_db" >/dev/null 2>&1 || true
	fi
	rm -rf "$tmp_dir"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

docker compose -f "$compose_file" exec -T postgres pg_isready -U themisy -d themisy >/dev/null
docker compose -f "$compose_file" exec -T postgres pg_dump -U themisy -d themisy --schema=themisy_runner --format=custom --no-owner > "$dump_file"
docker compose -f "$compose_file" exec -T postgres createdb -U themisy "$restore_db"
restore_created=1
docker compose -f "$compose_file" exec -T postgres pg_restore -U themisy -d "$restore_db" --no-owner < "$dump_file"

# The sentinels live only in the isolated restore database and are rolled back.
docker compose -f "$compose_file" exec -T postgres psql -X -v ON_ERROR_STOP=1 -U themisy -d "$restore_db" <<'SQL'
BEGIN;
INSERT INTO themisy_runner.runner_journal
    (grant_id, run_id, step_id, tenant_id, runner_group, nonce, idempotency_key,
     request_hash, target, action, status, state_version, created_at, updated_at, runner_id)
VALUES
    ('restore-probe', 'restore-probe', 'probe', 'restore-probe', 'restore-probe',
     'restore-probe-nonce', 'restore-probe-key', 'probe-hash', '{}', '{}',
     'RESERVED', 1, now(), now(), 'restore-probe-runner');

DO $verify$
DECLARE
    nonce_rejected boolean := false;
    key_rejected boolean := false;
BEGIN
    BEGIN
        INSERT INTO themisy_runner.runner_journal
            (grant_id, run_id, step_id, tenant_id, runner_group, nonce, idempotency_key,
             request_hash, target, action, status, state_version, created_at, updated_at, runner_id)
        VALUES
            ('restore-probe-2', 'restore-probe', 'probe', 'restore-probe', 'restore-probe',
             'restore-probe-nonce', 'restore-probe-key-2', 'probe-hash', '{}', '{}',
             'RESERVED', 1, now(), now(), 'restore-probe-runner');
    EXCEPTION WHEN unique_violation THEN
        nonce_rejected := true;
    END;
    IF NOT nonce_rejected THEN
        RAISE EXCEPTION 'restored nonce uniqueness constraint is missing';
    END IF;

    BEGIN
        INSERT INTO themisy_runner.runner_journal
            (grant_id, run_id, step_id, tenant_id, runner_group, nonce, idempotency_key,
             request_hash, target, action, status, state_version, created_at, updated_at, runner_id)
        VALUES
            ('restore-probe-3', 'restore-probe', 'probe', 'restore-probe', 'restore-probe',
             'restore-probe-nonce-3', 'restore-probe-key', 'probe-hash', '{}', '{}',
             'RESERVED', 1, now(), now(), 'restore-probe-runner');
    EXCEPTION WHEN unique_violation THEN
        key_rejected := true;
    END;
    IF NOT key_rejected THEN
        RAISE EXCEPTION 'restored idempotency uniqueness constraint is missing';
    END IF;
END
$verify$;
ROLLBACK;
SQL

printf '%s\n' "Isolated restore $restore_db passed nonce and idempotency uniqueness checks; temporary database will be removed."
