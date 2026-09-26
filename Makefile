.PHONY: fmt lint test test-integration test-integration-local test-coverage integration-db-up migrate build-api-lambda build-api-lambda-image build-dispatcher-lambda build-resource-controller-lambda terraform-fmt terraform-validate

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
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration ./... -count=1

test-coverage:
	@test -n "$${HAKO_TEST_DATABASE_URL:-}" || (echo "set HAKO_TEST_DATABASE_URL to a disposable PostgreSQL database" >&2; exit 1)
	go test -tags=integration -coverpkg=./... -coverprofile=coverage.out ./... -count=1
	go tool cover -func=coverage.out

integration-db-up:
	podman start hako-dev-postgres

migrate:
	go run ./cmd/hako-migrate

build-api-lambda:
	mkdir -p build/api-lambda
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o build/api-lambda/bootstrap ./cmd/hako-api
	cd build/api-lambda && zip -q ../hako-api.zip bootstrap

# Builds a single-platform Lambda image locally. ECR publishing is a separate, explicitly authorized step.
build-api-lambda-image:
	docker buildx build --platform linux/arm64 --provenance=false --load -f cmd/hako-api/Dockerfile -t hako-api:local .

build-dispatcher-lambda:
	mkdir -p build/dispatcher-lambda
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o build/dispatcher-lambda/bootstrap ./cmd/hako-dispatcher-lambda
	cd build/dispatcher-lambda && zip -q ../hako-dispatcher.zip bootstrap

build-resource-controller-lambda:
	mkdir -p build/resource-controller-lambda
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o build/resource-controller-lambda/bootstrap ./cmd/hako-resource-controller-lambda
	cd build/resource-controller-lambda && zip -q ../resource-controller-lambda.zip bootstrap

terraform-fmt:
	terraform fmt -check -recursive infra/terraform

terraform-validate:
	@set -e; \
	for root in infra/terraform/environments/dev/control-plane infra/terraform/environments/dev/resource-plane; do \
		terraform -chdir=$$root init -backend=false -input=false; \
		terraform -chdir=$$root validate; \
	done
