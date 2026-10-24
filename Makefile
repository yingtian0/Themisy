.PHONY: run compose-up compose-down test test-race test-integration test-identity-audit verify-runner-journal-backup-restore test-scenario test-policy test-adapter test-planner benchmark benchmark-planner lint vet check fmt fmt-check test-agent-cycle test-workflow-recovery test-localstack-sts

run:
	go run ./cmd/themisy

compose-up:
	docker compose -f deploy/compose/compose.yaml up -d

compose-down:
	docker compose -f deploy/compose/compose.yaml down

test:
	go test ./...

test-race:
	go test -race ./...

test-integration:
	set -eu; \
	docker compose -f deploy/compose/compose.yaml up -d postgres temporal; \
	THEMISY_INTEGRATION=1 go test -tags=integration -count=1 -p 1 ./test/integration ./internal/runner ./internal/transport ./internal/workflow; \
	THEMISY_INTEGRATION=1 THEMISY_TEST_POSTGRES_SERVICE_STOP=1 go test -tags=integration -count=1 -p 1 -run '^TestPostgresServiceStopFailsClosed$$' ./internal/runner; \
	$(MAKE) verify-runner-journal-backup-restore

test-workflow-recovery:
	./scripts/test-workflow-recovery.sh

verify-runner-journal-backup-restore:
	./scripts/verify-runner-journal-backup-restore.sh

test-identity-audit:
	bash scripts/test-identity-audit.sh

.PHONY: test-ci-observation-audit
test-ci-observation-audit:
	bash scripts/test-ci-observation-audit.sh

.PHONY: test-oidc-read-api
test-oidc-read-api:
	bash scripts/test-oidc-read-api.sh

test-localstack-sts:
	@test -n "$${THEMISY_LOCALSTACK_STS_ENDPOINT:-}" || { echo "THEMISY_LOCALSTACK_STS_ENDPOINT is required; live STS verification was not run" >&2; exit 1; }
	go test -race -tags=integration -count=1 -v ./pkg/credentials -run '^TestLocalStackSTSBrokerRoundTrip$$'

test-scenario:
	go test -count=1 ./test/scenario/...

test-policy:
	@if command -v opa >/dev/null 2>&1; then \
		opa test policies/baseline; \
	else \
		go test -count=1 ./internal/policy -run 'TestOPA'; \
	fi

test-adapter:
	go test -count=1 ./pkg/adapter ./pkg/credentials ./adapters/... ./internal/verification

test-planner:
	go test -run TestPlan -count=20 ./internal/planner

benchmark-planner:
	go test -bench . ./internal/planner

benchmark:
	go test -run '^$$' -bench . -benchmem ./internal/planner ./internal/scheduler

lint: vet

vet:
	go vet ./...

check: fmt-check test test-race lint test-agent-cycle

fmt:
	gofmt -w $$(find . -name '*.go' -type f)

fmt-check:
	@unformatted="$$(gofmt -l $$(find . -name '*.go' -type f))"; \
	if [ -n "$$unformatted" ]; then \
		echo "The following Go files are not formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

test-agent-cycle:
	python3 -m unittest discover -s tests/agent_cycle -v

.PHONY: test-policy-shadow-audit
test-policy-shadow-audit:
	bash scripts/test-policy-shadow-audit.sh

.PHONY: test-runtime-observation-audit
test-runtime-observation-audit:
	bash scripts/test-runtime-observation-audit.sh
