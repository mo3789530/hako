.PHONY: fmt lint test test-integration test-integration-local test-webhook-lifecycle-integration test-software-factory-e2e test-workload-runtime test-workload-runtime-integration test-coverage integration-db-up migrate build-api-lambda build-api-lambda-image build-dispatcher-lambda build-webhook-processor-lambda build-resource-controller-lambda build-health-reporter-lambda build-workload-controller-lambda build-workload-microvm-artifact build-workspace-microvm-image build-github-checks-consumer-image build-workspace-gateway-image terraform-fmt terraform-validate terraform-test

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

# Uses an isolated schema in the already-running local PostgreSQL database.
test-webhook-lifecycle-integration:
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration ./internal/api -run '^TestWebhookLifecycleE2E$$' -count=1

# Requires the cached pinned Go image and the already-running local PostgreSQL.
test-software-factory-e2e:
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" HAKO_E2E_REQUIRE_PODMAN=1 go test -tags=integration ./internal/workloadjobs -run '^TestSoftwareFactoryPodmanPostgresAndGitHubCheck$$' -count=1

test-workload-runtime:
	go test -race ./internal/workloadcontroller ./internal/workloadjournal ./internal/dispatcher

test-workload-runtime-integration:
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration ./internal/workloadcontroller ./internal/workloadjobs ./internal/store/workloads -count=1

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

build-webhook-processor-lambda:
	mkdir -p build/webhook-processor-lambda
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o build/webhook-processor-lambda/bootstrap ./cmd/hako-webhook-processor-lambda
	cd build/webhook-processor-lambda && zip -q ../hako-webhook-processor.zip bootstrap

build-resource-controller-lambda:
	mkdir -p build/resource-controller-lambda
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o build/resource-controller-lambda/bootstrap ./cmd/hako-resource-controller-lambda
	cd build/resource-controller-lambda && zip -q ../resource-controller-lambda.zip bootstrap

build-health-reporter-lambda:
	mkdir -p build/health-reporter-lambda
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o build/health-reporter-lambda/bootstrap ./cmd/hako-health-reporter-lambda
	cd build/health-reporter-lambda && zip -q ../hako-health-reporter.zip bootstrap

build-workload-controller-lambda:
	mkdir -p build/workload-controller-lambda
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o build/workload-controller-lambda/bootstrap ./cmd/hako-workload-controller-lambda
	cd build/workload-controller-lambda && zip -q ../hako-workload-controller.zip bootstrap

# Packages a Lambda MicroVM code artifact; the supplied file must contain public verification keys only.
build-workload-microvm-artifact:
	@test -n "$(HAKO_AGENT_TRUSTED_KEYS_FILE)" || (echo "set HAKO_AGENT_TRUSTED_KEYS_FILE to a public-key JSON file" >&2; exit 1)
	@test -s "$(HAKO_AGENT_TRUSTED_KEYS_FILE)" || (echo "trusted public-key JSON file is missing or empty" >&2; exit 1)
	mkdir -p build/workload-microvm
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o build/workload-microvm/hako-agent ./cmd/hako-agent
	cp images/workload-agent/Dockerfile build/workload-microvm/Dockerfile
	install -m 0644 "$(HAKO_AGENT_TRUSTED_KEYS_FILE)" build/workload-microvm/trusted-keys.json
	zip -q -j -FS build/hako-workload-microvm.zip build/workload-microvm/Dockerfile build/workload-microvm/hako-agent build/workload-microvm/trusted-keys.json

# Builds a distinct root-owned Workspace init image with a separate unprivileged shell Agent. Both the EFS helper RPM and public verification keys are pinned inputs.
build-workspace-microvm-image:
	@test -n "$(HAKO_EFS_UTILS_RPM_VERSION)" || (echo "set HAKO_EFS_UTILS_RPM_VERSION to the pinned amazon-efs-utils RPM version" >&2; exit 1)
	@test -n "$(HAKO_CODE_SERVER_VERSION)" || (echo "set HAKO_CODE_SERVER_VERSION to the pinned code-server version" >&2; exit 1)
	@test -n "$(HAKO_CODE_SERVER_SHA256)" || (echo "set HAKO_CODE_SERVER_SHA256 to the pinned ARM64 archive SHA-256" >&2; exit 1)
	@test -n "$(HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE)" || (echo "set HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE to a public-key JSON file" >&2; exit 1)
	@test -s "$(HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE)" || (echo "Workspace Agent public-key JSON file is missing or empty" >&2; exit 1)
	mkdir -p build/workspace-init
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o build/workspace-init/hako-workspace-init ./cmd/hako-workspace-init
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o build/workspace-init/hako-workspace-agent ./cmd/hako-workspace-agent
	install -m 0644 "$(HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE)" build/workspace-init/trusted-keys.json
	install -m 0644 images/workspace-init/Dockerfile build/workspace-init/Dockerfile
	podman build --arch arm64 --build-arg EFS_UTILS_RPM_VERSION="$(HAKO_EFS_UTILS_RPM_VERSION)" --build-arg CODE_SERVER_VERSION="$(HAKO_CODE_SERVER_VERSION)" --build-arg CODE_SERVER_SHA256="$(HAKO_CODE_SERVER_SHA256)" --tag hako-workspace-init:local -f build/workspace-init/Dockerfile build/workspace-init

# Builds an ARM64 ECS/Fargate image locally; publishing to ECR is a separate authorized step.
build-github-checks-consumer-image:
	podman build --arch arm64 --tag hako-checks-consumer:local -f cmd/hako-checks-consumer/Dockerfile .

# Builds a non-root ARM64 Gateway image locally; publication/deployment remain release operations.
build-workspace-gateway-image:
	podman build --arch arm64 --tag hako-workspace-gateway:local -f cmd/hako-workspace-gateway/Dockerfile .

terraform-fmt:
	terraform fmt -check -recursive infra/terraform

terraform-validate:
	@set -e; \
	for root in infra/terraform/environments/dev/control-plane infra/terraform/environments/dev/resource-plane; do \
		terraform -chdir=$$root init -backend=false -input=false; \
		terraform -chdir=$$root validate; \
	done

terraform-test:
	terraform -chdir=infra/terraform/modules/resource-plane init -backend=false -input=false
	terraform -chdir=infra/terraform/modules/resource-plane test
	terraform -chdir=infra/terraform/modules/workspace-storage init -backend=false -input=false
	terraform -chdir=infra/terraform/modules/workspace-storage test
