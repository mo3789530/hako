.PHONY: fmt lint test test-integration test-integration-local test-coverage integration-db-up integration-db-down migrate build-api-lambda terraform-fmt terraform-validate

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.git/*')

lint:
	go vet ./...

test:
	go test ./...

test-integration:
	@test -n "$${HAKO_TEST_DATABASE_URL:-}" || (echo "set HAKO_TEST_DATABASE_URL to a disposable PostgreSQL database" >&2; exit 1)
	go test -tags=integration ./... -count=1

test-integration-local:
	docker compose up -d --wait postgres
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration ./... -count=1

test-coverage:
	@test -n "$${HAKO_TEST_DATABASE_URL:-}" || (echo "set HAKO_TEST_DATABASE_URL to a disposable PostgreSQL database" >&2; exit 1)
	go test -tags=integration -coverpkg=./... -coverprofile=coverage.out ./... -count=1
	go tool cover -func=coverage.out

integration-db-up:
	docker compose up -d --wait postgres

integration-db-down:
	docker compose stop postgres

migrate:
	go run ./cmd/hako-migrate

build-api-lambda:
	mkdir -p build/api-lambda
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o build/api-lambda/bootstrap ./cmd/hako-api
	cd build/api-lambda && zip -q ../hako-api.zip bootstrap

terraform-fmt:
	terraform fmt -check -recursive infra/terraform

terraform-validate:
	@set -e; \
	for root in infra/terraform/environments/dev/control-plane infra/terraform/environments/dev/resource-plane; do \
		terraform -chdir=$$root init -backend=false -input=false; \
		terraform -chdir=$$root validate; \
	done
