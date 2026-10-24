#!/usr/bin/env bash
set -euo pipefail

# This target creates its own fixture. Never use a caller DATABASE_URL or Compose.
owned_container=""
cleanup() {
  local result=$?
  if [[ -n "$owned_container" ]]; then
    if ! docker rm -f -v "$owned_container" >/dev/null; then
      echo "runtime audit fixture cleanup failed" >&2
      result=1
    fi
  fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

owned_container=$(docker run -d --pull=missing \
  --tmpfs /var/lib/postgresql/data:rw,nosuid,nodev,size=256m \
  -e POSTGRES_USER=runtime_fixture -e POSTGRES_PASSWORD=runtime_fixture \
  -e POSTGRES_DB=themisy_runtime_audit_fixture \
  -p 127.0.0.1::5432 postgres:17.6-alpine)
port=$(docker inspect --format '{{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}' "$owned_container")
[[ "$port" =~ ^[0-9]+$ ]] || { echo "invalid runtime audit fixture port" >&2; exit 1; }
ready=false
for ((attempt=0; attempt<30; attempt++)); do
  if docker exec "$owned_container" pg_isready -h 127.0.0.1 -U runtime_fixture -d themisy_runtime_audit_fixture >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
[[ "$ready" == true ]] || { echo "runtime audit fixture did not become ready" >&2; exit 1; }

THEMISY_RUNTIME_AUDIT_DATABASE_URL="postgres://runtime_fixture:runtime_fixture@127.0.0.1:${port}/themisy_runtime_audit_fixture?sslmode=disable" \
  go test -race -tags=integration ./internal/verification -run '^TestPostgresRuntimeAudit$' -count=1 -v
