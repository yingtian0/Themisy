#!/usr/bin/env bash
# Dedicated local/CI infrastructure only; never reuse the caller's database.
set -euo pipefail
cd "$(dirname "$0")/.."

command -v docker >/dev/null
command -v go >/dev/null
docker info >/dev/null
scratch=$(mktemp -d "${TMPDIR:-/tmp}/themisy-recovery.XXXXXX")
suffix=${scratch##*.}
project="themisy-recovery-${suffix}"
pg_name="${project}-pg"
temporal_name="${project}-temporal"
network_id=""
pg_id=""
temporal_id=""
cleanup() {
  result=$?
  if [[ $result -ne 0 ]]; then
    [[ -z "$pg_id" ]] || docker logs "$pg_id" >"$scratch/postgres.log" 2>&1 || true
    [[ -z "$temporal_id" ]] || docker logs "$temporal_id" >"$scratch/temporal.log" 2>&1 || true
    echo "Recovery test failed; diagnostics retained in $scratch" >&2
  fi
  # IDs are assigned only after successful creation by this script. These
  # containers have no caller mounts; -v removes only their anonymous volumes.
  # Never delete by a potentially pre-existing name or run Compose down/prune.
  cleanup_failed=0
  for owned_container in "$temporal_id" "$pg_id"; do
    if [[ -n "$owned_container" ]] && ! docker rm -f -v "$owned_container" >/dev/null; then cleanup_failed=1; fi
  done
  if [[ -n "$network_id" ]] && ! docker network rm "$network_id" >/dev/null; then cleanup_failed=1; fi
  if [[ $cleanup_failed -ne 0 ]]; then
    echo "Owned recovery infrastructure cleanup failed" >&2
    if [[ $result -eq 0 ]]; then result=1; fi
  fi
  if [[ $result -eq 0 ]]; then
    rm -rf "$scratch"
  else
    echo "Recovery diagnostics retained in $scratch" >&2
  fi
  exit "$result"
}
trap cleanup EXIT
network_id=$(docker network create "$project")
pg_id=$(docker run -d --name "$pg_name" --network "$project" \
  -e POSTGRES_USER=themisy -e POSTGRES_PASSWORD=themisy -e POSTGRES_DB=themisy \
  -p 127.0.0.1::5432 postgres:17.6-alpine)
for attempt in {1..90}; do
  if docker exec "$pg_id" pg_isready -U themisy -d themisy >/dev/null 2>&1; then break; fi
  if [[ $attempt -eq 90 ]]; then echo "Dedicated PostgreSQL did not become ready" >&2; exit 1; fi
  sleep 1
done
temporal_id=$(docker run -d --name "$temporal_name" --network "$project" \
  -e DB=postgres12 -e DB_PORT=5432 -e POSTGRES_USER=themisy -e POSTGRES_PWD=themisy \
  -e POSTGRES_SEEDS="$pg_name" -e DBNAME=temporal -e VISIBILITY_DBNAME=temporal_visibility \
  -p 127.0.0.1::7233 temporalio/auto-setup:1.29.6.1)
for attempt in {1..90}; do
  # A successful read requires both frontend readiness and the default namespace.
  if docker exec "$temporal_id" temporal workflow list \
    --address "$temporal_name:7233" --namespace default --limit 1 >/dev/null 2>&1; then break; fi
  if [[ $attempt -eq 90 ]]; then echo "Dedicated Temporal namespace did not become ready" >&2; exit 1; fi
  sleep 1
done
pg_address=$(docker port "$pg_id" 5432/tcp)
temporal_address=$(docker port "$temporal_id" 7233/tcp)
[[ $pg_address =~ ^127\.0\.0\.1:[0-9]+$ ]]
[[ $temporal_address =~ ^127\.0\.0\.1:[0-9]+$ ]]
unset THEMISY_RECOVERY_TEST_PROCESS THEMISY_RECOVERY_TEST_QUEUE THEMISY_RECOVERY_TEST_READY_FILE THEMISY_RECOVERY_TEST_ROLLBACK
export THEMISY_DEPLOYMENT_RESULT_TEST_DATABASE_URL="postgres://themisy:themisy@${pg_address}/themisy?sslmode=disable"
export THEMISY_TEMPORAL_ADDRESS="$temporal_address"
echo "Running recovery acceptance on dedicated PostgreSQL/Temporal (including OS process crashes)" >&2
go test -race -tags=integration -count=1 -timeout=10m ./internal/workflow \
  -run '^(TestPostgresDeploymentSuccessResponseLoss|TestPostgresUnknownGrantRecoveryAfterWorkerReplacement|TestTemporalDeploymentCompletionLossAcrossWorkers|TestTemporalUnknownWaitCancelAcrossWorkersAndReplay|TestTemporalSigned(Grant|Rollback)RecoveryAcrossWorkers|TestTemporalSignedRecovery(Cancel)?AcrossProcesses|TestTemporalExpiredRecoveryCannotAuthorizeNextStep)$'
