#!/usr/bin/env bash
set -euo pipefail

# This target creates its own fixture. Never use a caller DATABASE_URL or Compose.
owned_container=""
cleanup() {
  local result=$?
  if [[ -n "$owned_container" ]]; then
    if ! docker rm -f -v "$owned_container" >/dev/null; then
      echo "OIDC read fixture cleanup failed" >&2
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
  -e POSTGRES_USER=oidc_read_fixture -e POSTGRES_PASSWORD=oidc_read_fixture \
  -e POSTGRES_DB=themisy_oidc_read_fixture \
  -p 127.0.0.1::5432 postgres:17.6-alpine)
port=$(docker inspect --format '{{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}' "$owned_container")
[[ "$port" =~ ^[0-9]+$ ]] || { echo "invalid OIDC read fixture port" >&2; exit 1; }
ready=false
for ((attempt=0; attempt<30; attempt++)); do
  if docker exec "$owned_container" pg_isready -h 127.0.0.1 -U oidc_read_fixture -d themisy_oidc_read_fixture >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
[[ "$ready" == true ]] || { echo "OIDC read fixture did not become ready" >&2; exit 1; }

THEMISY_OIDC_READ_DATABASE_URL="postgres://oidc_read_fixture:oidc_read_fixture@127.0.0.1:${port}/themisy_oidc_read_fixture?sslmode=disable" \
  go test -race -tags=integration ./internal/api -run '^TestPostgresOIDCReadAPI$' -count=1 -v
