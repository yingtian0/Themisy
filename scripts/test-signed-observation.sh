#!/usr/bin/env bash
set -euo pipefail

# This target creates its own fixture. Never use a caller DATABASE_URL or Compose.
owned_container=""
cleanup() {
  local result=$?
  if [[ -n "$owned_container" ]]; then
    if ! docker rm -f -v "$owned_container" >/dev/null; then
      echo "signed observation fixture cleanup failed" >&2
      result=1
    fi
  fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

owned_container=$(docker create --pull=missing \
  --tmpfs /var/lib/postgresql/data:rw,nosuid,nodev,size=256m \
  -e POSTGRES_USER=observation_fixture -e POSTGRES_PASSWORD=observation_fixture \
  -e POSTGRES_DB=themisy_observation_fixture \
  -p 127.0.0.1::5432 postgres:17.6-alpine)
docker start "$owned_container" >/dev/null
port=$(docker inspect --format '{{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}' "$owned_container")
[[ "$port" =~ ^[0-9]+$ ]] || { echo "invalid signed observation fixture port" >&2; exit 1; }
ready=false
for ((attempt=0; attempt<30; attempt++)); do
  if docker exec "$owned_container" pg_isready -h 127.0.0.1 -U observation_fixture -d themisy_observation_fixture >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
[[ "$ready" == true ]] || { echo "signed observation fixture did not become ready" >&2; exit 1; }

THEMISY_IDENTITY_DIRECTORY_TEST_DATABASE_URL="postgres://observation_fixture:observation_fixture@127.0.0.1:${port}/themisy_observation_fixture?sslmode=disable" \
THEMISY_OBSERVATION_TEST_DATABASE_URL="postgres://observation_fixture:observation_fixture@127.0.0.1:${port}/themisy_observation_fixture?sslmode=disable" \
  go test -race -tags=integration ./internal/application ./internal/store/postgres -run '^(TestPostgresSignedObservationFlow|TestPostgresPolicyGrantAdmission|TestIdentityDirectoryFenceSurvivesRestart|TestPolicyDistributionSurvivesRestart)$' -count=1 -v
