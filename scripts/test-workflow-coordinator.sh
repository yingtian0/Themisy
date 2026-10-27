#!/usr/bin/env bash
# This acceptance suite resets its database. Use only a fixture created here.
set -euo pipefail
cd "$(dirname "$0")/.."
owned_container=""
cleanup() {
  local result=$?
  if [[ -n "$owned_container" ]] && ! docker rm -f -v "$owned_container" >/dev/null; then
    echo "coordinator fixture cleanup failed" >&2
    result=1
  fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# The integration guard deliberately requires this loopback port and database.
# If the port is occupied, creation fails; never reuse or remove its owner.
owned_container=$(docker create --pull=missing \
  --tmpfs /var/lib/postgresql/data:rw,nosuid,nodev,size=256m \
  -e POSTGRES_USER=themisy -e POSTGRES_PASSWORD=themisy -e POSTGRES_DB=themisy \
  -p 127.0.0.1:56432:5432 postgres:17.6-alpine)
docker start "$owned_container" >/dev/null
ready=false
for ((attempt=0; attempt<30; attempt++)); do
  if docker exec "$owned_container" pg_isready -h 127.0.0.1 -U themisy -d themisy >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
[[ "$ready" == true ]] || { echo "coordinator fixture did not become ready" >&2; exit 1; }
unset THEMISY_COORDINATOR_WORKER_PROCESS THEMISY_WORKER_ID
THEMISY_INTEGRATION=1 THEMISY_COORDINATOR_INTEGRATION=1 \
  THEMISY_DATABASE_URL='postgres://themisy:themisy@127.0.0.1:56432/themisy?sslmode=disable' \
  go test -race -tags=integration -count=1 -timeout=3m -v ./test/integration \
    -run '^TestPostgresWorkflowCoordinator(AcrossIndependentStores|SeparateProcesses)$'
