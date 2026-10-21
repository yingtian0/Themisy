#!/bin/sh
# A fixture-only extension of the existing pg_dump/pg_restore operational drill.
set -eu
cd "$(dirname "$0")/.."

command -v docker >/dev/null
command -v go >/dev/null
docker info >/dev/null
backup_tmp_dir=$(mktemp -d)
backup_container_id=""
backup_stamp="$(date +%s)_$$"
source_db="themisy_backup_source_$backup_stamp"
restore_db="themisy_backup_restore_$backup_stamp"
schema_db="themisy_backup_restore_schema_$backup_stamp"
missing_db="themisy_backup_restore_missing_$backup_stamp"
empty_db="themisy_backup_source_empty_$backup_stamp"
source_created=0
restore_created=0
schema_created=0
missing_created=0
empty_created=0

cleanup() {
 backup_exit_status=$?
 # Only successful creates in this newly allocated container are eligible.
 for entry in "$source_created:$source_db" "$restore_created:$restore_db" "$schema_created:$schema_db" "$missing_created:$missing_db" "$empty_created:$empty_db"; do
  case "$entry" in
   1:*) docker exec "$backup_container_id" dropdb -U themisy "${entry#1:}" >/dev/null 2>&1 || true ;;
  esac
 done
 if [ -n "$backup_container_id" ]; then
  if [ "$backup_exit_status" -ne 0 ]; then docker logs "$backup_container_id" >"$backup_tmp_dir/postgres.log" 2>&1 || true; fi
  if ! docker rm -f -v "$backup_container_id" >/dev/null; then
   echo "Could not clean up owned container $backup_container_id" >&2
   backup_exit_status=1
  fi
 fi
 if [ "$backup_exit_status" -eq 0 ]; then rm -rf "$backup_tmp_dir"; else echo "Backup test diagnostics retained in $backup_tmp_dir" >&2; fi
 exit "$backup_exit_status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# Never attach to Compose or a caller-supplied customer database. tmpfs avoids
# leaving anonymous PostgreSQL data volumes after the disposable container exits.
backup_container_id=$(docker run -d --name "themisy-backup-$backup_stamp" \
 --tmpfs /var/lib/postgresql/data \
 -e POSTGRES_USER=themisy -e POSTGRES_PASSWORD=themisy -e POSTGRES_DB=themisy \
 -p 127.0.0.1::5432 postgres:17.6-alpine)
backup_ready=0
for attempt in $(seq 1 60); do
 if docker exec "$backup_container_id" pg_isready -U themisy -d themisy >/dev/null 2>&1; then backup_ready=1; break; fi
 sleep 1
done
if [ "$backup_ready" -ne 1 ]; then echo "Dedicated backup PostgreSQL did not become ready" >&2; exit 1; fi
backup_address=$(docker port "$backup_container_id" 5432/tcp)
case "$backup_address" in 127.0.0.1:*) ;; *) echo "Expected loopback-only PostgreSQL port" >&2; exit 1;; esac
case "${backup_address#127.0.0.1:}" in ''|*[!0-9]*) echo "Invalid PostgreSQL port" >&2; exit 1;; esac

docker exec "$backup_container_id" createdb -U themisy "$source_db"
source_created=1
export THEMISY_BACKUP_SOURCE_DATABASE_URL="postgres://themisy:themisy@$backup_address/$source_db?sslmode=disable"
THEMISY_BACKUP_RESTORE_DATABASE_URL='' go test -race -tags=integration -count=1 -v \
 ./internal/store/postgres -run '^TestPrepareRunnerBackupRetentionFixture$'
docker exec "$backup_container_id" pg_dump -U themisy -d "$source_db" \
 --schema=themisy_runner --format=custom --no-owner >"$backup_tmp_dir/runner.dump"

docker exec "$backup_container_id" createdb -U themisy "$restore_db"
restore_created=1
docker exec -i "$backup_container_id" pg_restore -U themisy -d "$restore_db" --no-owner <"$backup_tmp_dir/runner.dump"
THEMISY_BACKUP_RESTORE_DATABASE_URL="postgres://themisy:themisy@$backup_address/$restore_db?sslmode=disable" \
 go test -race -tags=integration -count=1 -v ./internal/store/postgres -run '^TestVerifyRunnerBackupRetention$'

# Retain the original fresh-probe uniqueness check, in a rolled-back transaction.
docker exec -i "$backup_container_id" psql -X -v ON_ERROR_STOP=1 -U themisy -d "$restore_db" <<'SQL'
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

# Negative controls are actual successful restores with missing data. An
# infrastructure error is not an expected mismatch and must not become PASS.
docker exec "$backup_container_id" createdb -U themisy "$schema_db"
schema_created=1
docker exec -i "$backup_container_id" pg_restore -U themisy -d "$schema_db" --schema-only --no-owner <"$backup_tmp_dir/runner.dump"
docker exec "$backup_container_id" createdb -U themisy "$missing_db"
missing_created=1
docker exec "$backup_container_id" pg_dump -U themisy -d "$source_db" \
 --schema=themisy_runner --exclude-table-data=themisy_runner.runner_journal \
 --format=custom --no-owner >"$backup_tmp_dir/missing-journal.dump"
docker exec -i "$backup_container_id" pg_restore -U themisy -d "$missing_db" --no-owner <"$backup_tmp_dir/missing-journal.dump"
for negative_db in "$schema_db" "$missing_db"; do
 if THEMISY_BACKUP_RESTORE_DATABASE_URL="postgres://themisy:themisy@$backup_address/$negative_db?sslmode=disable" \
  go test -race -tags=integration -count=1 -v ./internal/store/postgres -run '^TestVerifyRunnerBackupRetention$' >"$backup_tmp_dir/$negative_db.log" 2>&1; then
  echo "Missing backup data incorrectly passed retention verification" >&2
  exit 1
 fi
 if ! grep -q 'BACKUP_RETENTION_MISMATCH' "$backup_tmp_dir/$negative_db.log"; then
  cat "$backup_tmp_dir/$negative_db.log" >&2
  echo "Negative control failed for an unexpected reason" >&2
  exit 1
 fi
 echo "Expected retention mismatch detected: $negative_db"
done
# Both databases now contain matching empty application tables and the same
# migration rows. Without the nonempty source guard this could falsely pass.
docker exec "$backup_container_id" createdb -U themisy "$empty_db"
empty_created=1
docker exec -i "$backup_container_id" pg_restore -U themisy -d "$empty_db" --schema-only --no-owner <"$backup_tmp_dir/runner.dump"
docker exec "$backup_container_id" pg_dump -U themisy -d "$source_db" \
 --data-only --schema=themisy_runner --table=themisy_runner.schema_versions --no-owner >"$backup_tmp_dir/schema-versions.sql"
for empty_target in "$empty_db" "$schema_db"; do
 docker exec -i "$backup_container_id" psql -X -v ON_ERROR_STOP=1 -U themisy -d "$empty_target" <"$backup_tmp_dir/schema-versions.sql" >/dev/null
done
if THEMISY_BACKUP_SOURCE_DATABASE_URL="postgres://themisy:themisy@$backup_address/$empty_db?sslmode=disable" \
 THEMISY_BACKUP_RESTORE_DATABASE_URL="postgres://themisy:themisy@$backup_address/$schema_db?sslmode=disable" \
 go test -race -tags=integration -count=1 -v ./internal/store/postgres -run '^TestVerifyRunnerBackupRetention$' >"$backup_tmp_dir/empty-source.log" 2>&1; then
 echo "Matching empty source/restore incorrectly passed retention verification" >&2
 exit 1
fi
if ! grep -q 'SOURCE_FIXTURE_INVALID' "$backup_tmp_dir/empty-source.log"; then
 cat "$backup_tmp_dir/empty-source.log" >&2
 echo "Empty-source control failed for an unexpected reason" >&2
 exit 1
fi
echo 'Matching empty source/restore rejected by nonempty fixture precondition.'
printf '%s\n' 'Backup/restore retained existing SUCCEEDED/UNKNOWN Journal, signed Manifest/revision and Audit; old nonce/idempotency and all data-loss controls verified.'
