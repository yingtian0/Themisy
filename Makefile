.PHONY: run compose-up compose-down test test-race test-integration test-scenario test-policy test-adapter test-planner benchmark benchmark-planner lint vet check fmt fmt-check

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
	docker compose -f deploy/compose/compose.yaml up -d postgres temporal
	THEMISY_INTEGRATION=1 go test -tags=integration -count=1 ./test/integration ./internal/runner

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

check: fmt-check test test-race lint

fmt:
	gofmt -w $$(find . -name '*.go' -type f)

fmt-check:
	@unformatted="$$(gofmt -l $$(find . -name '*.go' -type f))"; \
	if [ -n "$$unformatted" ]; then \
		echo "The following Go files are not formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
