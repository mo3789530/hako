.PHONY: check-local-workspace-microvm-host smoke-local-workspace-microvm-boot smoke-local-workspace-controller test-local-workspace-reconciler build-local-workspace-reconciler test-local-workspace-worker build-local-workspace-worker test-local-idp build-local-idp test-local-workspace-keygen build-local-workspace-keygen test-local-workspace-profile build-local-workspace-stack build-local-workspace-initrd test-local-workspace-initrd test-local-workspace-initrd-rekey test-managed-runtime-local test-managed-runtime-integration fmt lint test test-integration test-integration-local test-webhook-lifecycle-integration test-tenant-app-proof-integration test-tenant-github-apps-integration test-actions-runner-lifecycle-integration test-software-factory-e2e test-native-agent-e2e test-api-lambda-image-smoke test-api-lambda-rie-smoke test-aws-e2e-guard aws-e2e-preflight test-workload-runtime test-workload-runtime-integration test-job-services-podman test-workspace-service-runtime-integration test-workspace-podman-network test-artifact-retention-integration test-workspace-idle-lifecycle-integration test-preview-expiry-integration test-coverage integration-db-up migrate build-api-lambda build-api-lambda-image build-dispatcher-lambda build-webhook-processor-lambda build-resource-controller-lambda build-workspace-service-controller-lambda build-health-reporter-lambda build-workspace-idle-reconciler-lambda build-preview-expiry-lambda build-preview-runtime-dispatch-lambda build-preview-result-consumer-lambda build-workload-controller-lambda build-actions-runner-lambda build-actions-runner-microvm-image build-workload-microvm-artifact verify-workload-microvm-artifact build-workload-microvm-services-image build-workspace-microvm-image build-github-checks-consumer-image build-github-broker-image build-workspace-gateway-image check-arm64-podman-build terraform-fmt terraform-validate terraform-test test-local-workspace-cli-firecracker

TERRAFORM ?= terraform
FIRECRACKER ?= firecracker
FIRECRACKER_KERNEL ?= /boot/vmlinuz-$(shell uname -r)
FIRECRACKER_INITRD ?= build/local-workspace/workspace-initrd.cpio.gz
FIRECRACKER_KERNEL_CONFIG ?= /boot/config-$(shell uname -r)
FIRECRACKER_VSOCK_MODULE_DIR ?= /lib/modules/$(shell uname -r)
.PHONY: build-local-workspace-native-rootfs check-workspace-image-build-host smoke-local-workspace-hako-initrd

check-local-workspace-microvm-host:
	FIRECRACKER_BIN="$(FIRECRACKER)" ./scripts/check-local-workspace-microvm-host.sh

# Boots a disposable static Guest init on the host kernel through Firecracker/KVM.
smoke-local-workspace-microvm-boot:
	FIRECRACKER_BIN="$(FIRECRACKER)" ./scripts/smoke-local-firecracker-boot.sh

# Exercises the private local operation API against a real Firecracker Guest.
smoke-local-workspace-controller: build-local-workspace-reconciler
	python3 scripts/smoke-local-workspace-controller.py "$(FIRECRACKER)"

# Runs the Hako CLI exec client through a real local Gateway and Firecracker Guest.
test-local-workspace-cli-firecracker:
	@test -n "$(HAKO_LOCAL_WORKSPACE_AGENT_PRIVATE_KEY)" || (echo "set HAKO_LOCAL_WORKSPACE_AGENT_PRIVATE_KEY to the matching local Guest key" >&2; exit 1)
	HAKO_LOCAL_WORKSPACE_AGENT_PRIVATE_KEY="$(HAKO_LOCAL_WORKSPACE_AGENT_PRIVATE_KEY)" FIRECRACKER_BIN="$(FIRECRACKER)" FIRECRACKER_KERNEL="$(FIRECRACKER_KERNEL)" FIRECRACKER_INITRD="$(FIRECRACKER_INITRD)" go test ./cmd/hako -tags firecracker_workspace_smoke -run '^TestWorkspaceExecCLIThroughFirecrackerGuest$$' -count=1 -v

# Boots the built native Workspace initrd and probes its real Agent over vsock.
smoke-local-workspace-hako-initrd:
	FIRECRACKER_BIN="$(FIRECRACKER)" FIRECRACKER_KERNEL_CONFIG="$(FIRECRACKER_KERNEL_CONFIG)" FIRECRACKER_VSOCK_MODULE_DIR="$(FIRECRACKER_VSOCK_MODULE_DIR)" ./scripts/smoke-local-workspace-hako-initrd.sh

# Runs unit/race coverage for the host-local lifecycle and startup reconciler.
test-local-workspace-reconciler:
	GOCACHE="$${GOCACHE:-/tmp/hako-go-cache}" go test -race ./internal/workspaceruntime ./cmd/hako-local-workspace-reconciler -count=1

# Runs unit/race coverage for the local Outbox-to-Controller-to-PostgreSQL worker.
test-local-workspace-worker:
	GOCACHE="$${GOCACHE:-/tmp/hako-go-cache}" go test -race ./internal/localoperationworker ./cmd/hako-local-workspace-worker ./internal/store/outbox -count=1

# Builds the Linux host-local startup reconciler; no AWS configuration is used.
build-local-workspace-reconciler:
	go build -trimpath -o build/hako-local-workspace-reconciler ./cmd/hako-local-workspace-reconciler

# Builds the AWS-free local Workspace Outbox worker.
build-local-workspace-worker:
	go build -trimpath -o build/hako-local-workspace-worker ./cmd/hako-local-workspace-worker

# Runs race-enabled tests for the loopback-only local OAuth/JWKS provider.
test-local-idp:
	GOCACHE="$${GOCACHE:-/tmp/hako-go-cache}" go test -race ./internal/localidp ./cmd/hako-local-idp -count=1

# Builds the development-only loopback identity provider.
build-local-idp:
	go build -trimpath -o build/hako-local-idp ./cmd/hako-local-idp

# Tests/builds the private single-host session, Agent, HMAC and localhost TLS key bundle generator.
test-local-workspace-keygen:
	GOCACHE="$${GOCACHE:-/tmp/hako-go-cache}" go test -race ./cmd/hako-local-workspace-keygen -count=1

build-local-workspace-keygen:
	go build -trimpath -o build/hako-local-workspace-keygen ./cmd/hako-local-workspace-keygen

# Builds the local Control Plane/CLI/IDP/Gateway/Outbox worker/KVM controller processes.
build-local-workspace-stack: build-local-workspace-keygen build-local-idp build-local-workspace-worker build-local-workspace-reconciler
	mkdir -p build
	go build -trimpath -o build/hako-api ./cmd/hako-api
	go build -trimpath -o build/hako-workspace-gateway ./cmd/hako-workspace-gateway
	go build -trimpath -o build/hako ./cmd/hako

test-local-workspace-profile:
	python3 scripts/test-local-workspace-launch-profile.py

# Packages an extracted native-architecture Workspace image rootfs as a Firecracker initrd.
build-local-workspace-initrd:
	@test -n "$(HAKO_LOCAL_WORKSPACE_ROOTFS_DIR)" || (echo "set HAKO_LOCAL_WORKSPACE_ROOTFS_DIR to the extracted native-architecture Workspace image rootfs" >&2; exit 1)
	mkdir -p build/local-workspace
	@case "$$(uname -m)" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "unsupported host architecture: $$(uname -m)" >&2; exit 1 ;; esac; \
	GOOS=linux GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o build/local-workspace/hako-local-workspace-guest-init ./cmd/hako-local-workspace-guest-init
	@if [ "$(HAKO_LOCAL_WORKSPACE_INCLUDE_HOST_MODULES)" = true ]; then \
	  ./scripts/package-local-workspace-initrd.sh "$(HAKO_LOCAL_WORKSPACE_ROOTFS_DIR)" build/local-workspace/hako-local-workspace-guest-init build/local-workspace/workspace-initrd.cpio.gz "$(FIRECRACKER_VSOCK_MODULE_DIR)" "$(FIRECRACKER_KERNEL_CONFIG)"; \
	else \
	  ./scripts/package-local-workspace-initrd.sh "$(HAKO_LOCAL_WORKSPACE_ROOTFS_DIR)" build/local-workspace/hako-local-workspace-guest-init build/local-workspace/workspace-initrd.cpio.gz; \
	fi

# Builds and exports a host-native Workspace OCI rootfs, then packages it as a Guest initrd.
build-local-workspace-native-rootfs:
	@set -e; case "$$(uname -m)" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "unsupported host architecture: $$(uname -m)" >&2; exit 1 ;; esac; \
	$(MAKE) build-workspace-microvm-image HAKO_WORKSPACE_IMAGE_ARCH=$$arch HAKO_WORKSPACE_IMAGE_TAG=localhost/hako-workspace-init:local-native; \
	rootfs=$$(mktemp -d "$${TMPDIR:-/tmp}/hako-local-workspace-rootfs.XXXXXX"); cid=; \
	cleanup() { if [ -n "$$cid" ]; then podman rm -f "$$cid" >/dev/null 2>&1 || true; fi; podman unshare rm -rf "$$rootfs" >/dev/null 2>&1 || true; }; \
	trap cleanup EXIT HUP INT TERM; cid=$$(podman create localhost/hako-workspace-init:local-native); \
	podman export "$$cid" | podman unshare tar -C "$$rootfs" -xf -; \
	podman rm "$$cid" >/dev/null; cid=; \
	podman unshare $(MAKE) build-local-workspace-initrd HAKO_LOCAL_WORKSPACE_ROOTFS_DIR="$$rootfs" HAKO_LOCAL_WORKSPACE_INCLUDE_HOST_MODULES=true

test-local-workspace-initrd:
	./scripts/test-local-workspace-initrd-packaging.sh

test-local-workspace-initrd-rekey:
	python3 scripts/test_local_workspace_initrd_rekey.py

# ARM64 Podman builds execute RUN commands from the target image. On x86_64,
# fail early unless a QEMU binfmt_misc handler is already installed.
check-arm64-podman-build:
	@host_arch=$$(uname -m); \
	if [ "$$host_arch" = "aarch64" ]; then exit 0; fi; \
	for handler in /proc/sys/fs/binfmt_misc/qemu-aarch64 /proc/sys/fs/binfmt_misc/qemu-aarch64-static; do \
		if [ -r "$$handler" ] && grep -q '^enabled$$' "$$handler"; then exit 0; fi; \
	done; \
	echo "ARM64 Podman image builds require an aarch64 host or enabled qemu-aarch64 binfmt_misc handler" >&2; \
	exit 1

HAKO_WORKSPACE_SERVICES_ENABLED ?= false
HAKO_WORKSPACE_AGENT_VSOCK_ENABLED ?= false
HAKO_PREVIEW_TICKET_KEY_ID ?=

.PHONY: validate-workspace-agent-keys
validate-workspace-agent-keys:
	@test -n "$(HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE)" -a -s "$(HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE)" || (echo "set HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE to a non-empty public-key JSON file" >&2; exit 1)
	go run ./cmd/hako-workspace-keycheck -file "$(HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE)" -require-id "$(HAKO_PREVIEW_TICKET_KEY_ID)"

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.git/*')

lint:
	go vet ./...

test:
	go test ./...

test-integration:
	@test -n "$${HAKO_TEST_DATABASE_URL:-}" || (echo "set HAKO_TEST_DATABASE_URL to a disposable PostgreSQL database" >&2; exit 1)
	go test -tags=integration -p=1 ./... -count=1

test-integration-local:
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration -p=1 ./... -count=1

# Uses an isolated schema in the already-running local PostgreSQL database.
test-webhook-lifecycle-integration:
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration ./internal/api -run '^TestWebhookLifecycleE2E$$' -count=1

# Synthetic App pins and fake GitHub proof; isolated schemas; leaves PostgreSQL running.
test-tenant-app-proof-integration:
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration ./internal/api ./internal/githubbroker -run 'TenantAppMetadata|TenantAppVerify|TenantAppProof' -count=1

# Two-Tenant setup, webhook, Run pin, checkout Broker, and draft proof acceptance.
test-tenant-github-apps-integration:
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration ./internal/integration ./internal/api ./internal/githubbroker ./internal/store/githubregistry ./internal/store/githubwebhook -run 'TenantGitHubAppTwoTenant|TenantSetupCallback|TenantAppProof|CheckoutResolver|ResolveAppProofCredential|TenantWebhook' -count=1

# Synthetic GitHub/provider credentials; isolated schemas; leaves PostgreSQL running.
test-actions-runner-lifecycle-integration:
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration ./internal/actionsrunner ./internal/api ./internal/store/githubwebhook ./internal/githubbroker ./internal/githubchecks -run 'ActionsRunner|RunnerGuestRuntime|CleanupWorker|BrokerMTLS' -count=1

# Requires the cached pinned Go image and the already-running local PostgreSQL.
test-software-factory-e2e:
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" HAKO_E2E_REQUIRE_PODMAN=1 go test -tags=integration ./internal/workloadjobs -run '^TestSoftwareFactoryPodmanPostgresAndGitHubCheck$$' -count=1
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration ./internal/api -run '^TestWorkloadRunRoutesEnforceTenantAndRequesterVisibility$$' -count=1
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration ./internal/workloadcontroller -run '^TestComposedLambdaWorker(ResultAndCleanupProjectsIntoPostgres|CancellationOutboxTerminatesAndProjectsIntoPostgres)$$' -count=1

.PHONY: check-local-workspace-microvm-host test-managed-runtime-local test-software-factory-kiri build-agentcore-coding-agent-image build-agentcore-worker-lambda
test-software-factory-kiri: export HAKO_E2E_USE_KIRI = 1
test-software-factory-kiri: test-software-factory-e2e

# Builds the digest-pinned, non-root ARM64 AgentCore Runtime image with Kiri; this never publishes it.
build-agentcore-coding-agent-image:
	kiri build --platform linux/arm64 --tag hako-coding-agent:local -f coding-agent/Dockerfile coding-agent

build-agentcore-worker-lambda:
	mkdir -p build/agentcore-worker-lambda
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o build/agentcore-worker-lambda/bootstrap ./cmd/hako-agentcore-worker-lambda
	cd build/agentcore-worker-lambda && zip -q ../hako-agentcore-worker.zip bootstrap

# Composes the signed Issue command inbox, persisted bounded Agent task, and local Native Agent proposal executor.
test-native-agent-e2e:
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration ./internal/workloadjobs -run '^TestIssueCommandToNativeAgentProposalUsesPersistedBoundedContext$$' -count=1

# Local unit acceptance for managed routes. No AWS, GitHub, or PostgreSQL required.
test-managed-runtime-local:
	GOCACHE="$${GOCACHE:-/tmp/hako-go-cache}" go test ./internal/workloadagentcore ./internal/actionsrunner ./internal/workspacesession ./internal/workspacegateway ./internal/workspaceruntime -count=1
	PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=coding-agent/src python3 -m unittest discover -s coding-agent/tests -v

# PostgreSQL-backed fake-provider lifecycle acceptance. Never calls AWS/GitHub.
test-managed-runtime-integration: test-actions-runner-lifecycle-integration test-workspace-idle-lifecycle-integration

test-workload-runtime:
	go test -race ./internal/workloadcontroller ./internal/workloadjournal ./internal/dispatcher

test-workload-runtime-integration:
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration ./internal/workloadcontroller ./internal/workloadjobs ./internal/store/workloads -count=1

# Uses the production Job Service engine with a Podman CLI adapter and a local test image.
test-job-services-podman:
	HAKO_JOB_SERVICES_PODMAN_TEST=1 go test -tags=integration ./internal/workloadagent -run '^TestNerdctlJobServicePodmanRuntimeContract$$' -count=1

# Covers Workspace Service Guest/Controller behavior plus its API/Outbox/report lifecycle.
# The PostgreSQL test requires the already-running local development database.
test-workspace-service-runtime-integration:
	go test ./internal/workspaceagent ./internal/workspaceruntime -count=1
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration ./internal/api -run '^TestTenantMembershipRouteResolvesUserAndRejectsNonMembers$$' -count=1

# Host-only Podman evidence for --internal bridge egress; this is not a
# substitute for the nerdctl/CNI or Lambda MicroVM acceptance gates.
test-workspace-podman-network:
	./scripts/test-workspace-podman-network.sh

# Exercises the signed Resource Plane cleanup API and cross-Plane metadata fence against local PostgreSQL.
test-artifact-retention-integration:
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration ./internal/api -run '^TestResourcePlaneArtifactCleanupIsSignedPlaneScopedAndIdempotent$$' -count=1

# Composes Gateway WSS, signed connection lease API, PostgreSQL idle sweep, and CLI resume option tests.
test-workspace-idle-lifecycle-integration:
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration ./internal/api ./cmd/hako -run 'TestWorkspaceGatewayDisconnectReconnectFencesIdleSuspendEndToEnd|TestParseWorkspaceActionOptions' -count=1

test-preview-expiry-integration:
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration -cover ./internal/store/previews ./internal/previewcleanup ./internal/previewprovision ./internal/previewdispatch ./internal/previewcontroller ./internal/workloadjobs ./internal/api ./internal/githubbroker -run 'TestCleanupLeaseBackoffRecoveryAndFencedCompletion|TestPreviewCleanupWorkerRetriesAndOnlyReleasesQuotaAfterConfirmation|TestRuntimeWorkerCleansRetiredGenerationsAndReleasesPreviewQuota|TestWorkerFencesAndCompensatesStalePreviewProvision|TestPreviewCommandProducerOutboxAndDispatcherEndToEnd|TestScheduledWorkerSelectsTenantEligibleCapacityAndReleasesAfterCleanup|TestRepositoryPreviewListingAndExpiryBatch|TestPreviewRuntimeGenerationPinsTenantGitHubAppCredentialIdentity|TestPreviewRuntimeGeneration|TestPreviewSourceMaterializer|TestRepositoryPreviewWebhookLifecycleFencesStalePRUpdates|TestRepositoryPreviewConcurrentAdmissionRespectsTenantQuota|TestRepositoryPreviewQuotaConfigurationIncludesPRReservations|TestRepositoryFactorySettingRequiresTenantOwnerAndActiveRegisteredRepository|TestRepositoryPreviewSessionIsTenantScopedAndFencedByPreviewLifecycle|TestPostgresPreviewCheckoutResolverRequiresCurrentClaimAndPinnedTenantApp' -count=1

test-coverage:
	@test -n "$${HAKO_TEST_DATABASE_URL:-}" || (echo "set HAKO_TEST_DATABASE_URL to a disposable PostgreSQL database" >&2; exit 1)
	HAKO_E2E_REQUIRE_PODMAN=1 HAKO_JOB_SERVICES_PODMAN_TEST=1 go test -tags=integration -p=1 -coverpkg=./... -coverprofile=coverage.out ./... -count=1
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

# Builds/runs the OCI HTTP server on the local host architecture against local PostgreSQL.
test-api-lambda-image-smoke:
	set -e; trap 'podman image rm hako-api:smoke >/dev/null 2>&1 || true' EXIT; \
		podman build --arch amd64 --build-arg TARGETARCH=amd64 -f cmd/hako-api/Dockerfile -t hako-api:smoke .; \
		HAKO_DATABASE_URL="$${HAKO_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" HAKO_API_SMOKE_IMAGE=hako-api:smoke ./scripts/test-api-lambda-image-smoke.sh

# Uses AWS's Runtime Interface Emulator to exercise the image's Lambda Web Adapter with API Gateway v2 events.
test-api-lambda-rie-smoke:
	set -e; trap 'podman image rm hako-api:rie-local hako-api:rie-smoke >/dev/null 2>&1 || true' EXIT; \
		podman build --arch amd64 --build-arg TARGETARCH=amd64 -f cmd/hako-api/Dockerfile -t hako-api:rie-smoke .; \
		podman build --arch amd64 -f cmd/hako-api/Dockerfile.rie-smoke -t hako-api:rie-local .; \
		HAKO_DATABASE_URL="$${HAKO_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" HAKO_API_SMOKE_IMAGE=hako-api:rie-local ./scripts/test-api-lambda-rie-smoke.sh

# Pure validation and mocked STS lookups; never contacts AWS.
test-aws-e2e-guard:
	go test ./internal/awse2e ./cmd/hako-aws-e2e-preflight -count=1

# Read-only STS identity check; requires explicit HAKO_AWS_E2E_* opt-in/configuration.
aws-e2e-preflight:
	go run ./cmd/hako-aws-e2e-preflight preflight

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

build-workspace-service-controller-lambda:
	mkdir -p build/workspace-service-controller-lambda
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o build/workspace-service-controller-lambda/bootstrap ./cmd/hako-workspace-service-controller-lambda
	cd build/workspace-service-controller-lambda && zip -q ../workspace-service-controller-lambda.zip bootstrap

build-health-reporter-lambda:
	mkdir -p build/health-reporter-lambda
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o build/health-reporter-lambda/bootstrap ./cmd/hako-health-reporter-lambda
	cd build/health-reporter-lambda && zip -q ../hako-health-reporter.zip bootstrap

build-workspace-idle-reconciler-lambda:
	mkdir -p build/workspace-idle-reconciler
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o build/workspace-idle-reconciler/bootstrap ./cmd/hako-workspace-idle-reconciler-lambda
	cd build/workspace-idle-reconciler && zip -q ../hako-workspace-idle-reconciler.zip bootstrap

build-preview-expiry-lambda:
	mkdir -p build/preview-expiry
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o build/preview-expiry/bootstrap ./cmd/hako-preview-expiry-lambda
	cd build/preview-expiry && zip -q ../hako-preview-expiry.zip bootstrap

build-preview-runtime-dispatch-lambda:
	mkdir -p build/preview-runtime-dispatch
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o build/preview-runtime-dispatch/bootstrap ./cmd/hako-preview-runtime-dispatch-lambda
	cd build/preview-runtime-dispatch && zip -q ../hako-preview-runtime-dispatch.zip bootstrap

build-preview-result-consumer-lambda:
	mkdir -p build/preview-result-consumer
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o build/preview-result-consumer/bootstrap ./cmd/hako-preview-result-consumer-lambda
	cd build/preview-result-consumer && zip -q ../hako-preview-result-consumer.zip bootstrap

build-workload-controller-lambda:
	mkdir -p build/workload-controller-lambda
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o build/workload-controller-lambda/bootstrap ./cmd/hako-workload-controller-lambda
	cd build/workload-controller-lambda && zip -q ../hako-workload-controller.zip bootstrap

build-actions-runner-lambda:
	mkdir -p build/actions-runner-lambda
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o build/actions-runner-lambda/bootstrap ./cmd/hako-actions-runner-lambda
	cd build/actions-runner-lambda && zip -q ../hako-actions-runner.zip bootstrap

# Builds the non-root ARM64 Lambda MicroVM guest image on top of the official, digest-pinned GitHub Runner image. The only caller-provided key is public.
build-actions-runner-microvm-image: check-arm64-podman-build
	@test -n "$(HAKO_ACTIONS_RUNNER_BOOTSTRAP_PUBLIC_KEY_FILE)" || (echo "set HAKO_ACTIONS_RUNNER_BOOTSTRAP_PUBLIC_KEY_FILE to a PUBLIC KEY PEM" >&2; exit 1)
	@test -s "$(HAKO_ACTIONS_RUNNER_BOOTSTRAP_PUBLIC_KEY_FILE)"
	@grep -q '^-----BEGIN PUBLIC KEY-----$$' "$(HAKO_ACTIONS_RUNNER_BOOTSTRAP_PUBLIC_KEY_FILE)"
	@! grep -q 'PRIVATE KEY' "$(HAKO_ACTIONS_RUNNER_BOOTSTRAP_PUBLIC_KEY_FILE)"
	mkdir -p build/actions-runner-microvm
	cp "$(HAKO_ACTIONS_RUNNER_BOOTSTRAP_PUBLIC_KEY_FILE)" build/actions-runner-microvm/actions-runner-bootstrap.pub
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o build/actions-runner-microvm/hako-actions-runner-guest ./cmd/hako-actions-runner-guest
	cp images/actions-runner/Dockerfile build/actions-runner-microvm/Dockerfile
	podman build --arch arm64 --tag hako-actions-runner:local -f build/actions-runner-microvm/Dockerfile build/actions-runner-microvm

# Packages a Lambda MicroVM code artifact; the supplied file must contain public verification keys only.
build-workload-microvm-artifact:
	@test -n "$(HAKO_AGENT_TRUSTED_KEYS_FILE)" || (echo "set HAKO_AGENT_TRUSTED_KEYS_FILE to a public-key JSON file" >&2; exit 1)
	@test -s "$(HAKO_AGENT_TRUSTED_KEYS_FILE)" || (echo "trusted public-key JSON file is missing or empty" >&2; exit 1)
	mkdir -p build/workload-microvm
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o build/workload-microvm/hako-agent ./cmd/hako-agent
	cp images/workload-agent/Dockerfile build/workload-microvm/Dockerfile
	install -m 0644 "$(HAKO_AGENT_TRUSTED_KEYS_FILE)" build/workload-microvm/trusted-keys.json
	zip -q -j -FS build/hako-workload-microvm.zip build/workload-microvm/Dockerfile build/workload-microvm/hako-agent build/workload-microvm/trusted-keys.json

# Rebuild and verify the deployable ZIP's exact contents, key format, and static ARM64 Agent binary.
verify-workload-microvm-artifact: build-workload-microvm-artifact
	@set -eu; \
	go run ./cmd/hako-agent-keycheck -file "$(HAKO_AGENT_TRUSTED_KEYS_FILE)"; \
	tmpdir=$$(mktemp -d); trap 'rm -rf "$$tmpdir"' EXIT HUP INT TERM; \
	unzip -q build/hako-workload-microvm.zip -d "$$tmpdir"; \
	actual=$$(find "$$tmpdir" -mindepth 1 -maxdepth 1 -type f -printf '%f\n' | sort); \
	expected=$$(printf '%s\n' Dockerfile hako-agent trusted-keys.json | sort); \
	test "$$actual" = "$$expected" || { echo "unexpected workload artifact ZIP contents" >&2; exit 1; }; \
	cmp "$(HAKO_AGENT_TRUSTED_KEYS_FILE)" "$$tmpdir/trusted-keys.json"; \
	readelf -h "$$tmpdir/hako-agent" > "$$tmpdir/elf-header.txt"; \
	grep -Eq 'Class:[[:space:]]+ELF64' "$$tmpdir/elf-header.txt"; \
	grep -Eq 'Machine:[[:space:]]+AArch64' "$$tmpdir/elf-header.txt"; \
	readelf -l "$$tmpdir/hako-agent" > "$$tmpdir/program-headers.txt"; \
	if grep -q INTERP "$$tmpdir/program-headers.txt"; then echo "workload Agent must be statically linked" >&2; exit 1; fi; \
	echo "verified workload MicroVM artifact: exact ZIP contents, public keys, static ELF64/AArch64 Agent"

# Builds an opt-in Lambda MicroVM image variant with a separately-owned
# rootless Job containerd daemon. Services remain disabled in the image.
build-workload-microvm-services-image: check-arm64-podman-build build-workload-microvm-artifact
	@test -n "$(HAKO_NERDCTL_FULL_ARCHIVE)" || (echo "set HAKO_NERDCTL_FULL_ARCHIVE to a reviewed nerdctl-full Linux ARM64 archive" >&2; exit 1)
	@test -n "$(HAKO_NERDCTL_FULL_VERSION)" || (echo "set HAKO_NERDCTL_FULL_VERSION to the exact nerdctl release" >&2; exit 1)
	@test -n "$(HAKO_NERDCTL_FULL_SHA256)" || (echo "set HAKO_NERDCTL_FULL_SHA256 to the reviewed archive digest" >&2; exit 1)
	@test -n "$(HAKO_CNI_PLUGINS_VERSION)" || (echo "set HAKO_CNI_PLUGINS_VERSION to the exact CNI plugins version in nerdctl-full" >&2; exit 1)
	@test -s "$(HAKO_NERDCTL_FULL_ARCHIVE)" || (echo "nerdctl-full archive is missing or empty" >&2; exit 1)
	@test -n "$(HAKO_SLIRP4NETNS_BINARY)" || (echo "set HAKO_SLIRP4NETNS_BINARY to a reviewed Linux ARM64 slirp4netns binary" >&2; exit 1)
	@test -n "$(HAKO_SLIRP4NETNS_VERSION)" || (echo "set HAKO_SLIRP4NETNS_VERSION to the exact slirp4netns release" >&2; exit 1)
	@test -n "$(HAKO_SLIRP4NETNS_SHA256)" || (echo "set HAKO_SLIRP4NETNS_SHA256 to the reviewed binary digest" >&2; exit 1)
	@test -s "$(HAKO_SLIRP4NETNS_BINARY)" || (echo "slirp4netns binary is missing or empty" >&2; exit 1)
	@set -e; archive_entries=$$(tar -tzf "$(HAKO_NERDCTL_FULL_ARCHIVE)"); \
	for required in bin/nerdctl bin/containerd bin/containerd-rootless.sh bin/rootlesskit libexec/cni/firewall share/doc/nerdctl-full/README.md; do \
		printf '%s\n' "$$archive_entries" | grep -Fxq "$$required" || { echo "nerdctl-full archive is missing $$required" >&2; exit 1; }; \
	done
	cp "$(HAKO_NERDCTL_FULL_ARCHIVE)" build/workload-microvm/nerdctl-full-linux-arm64.tar.gz
	cp "$(HAKO_SLIRP4NETNS_BINARY)" build/workload-microvm/slirp4netns-aarch64
	cp images/workload-agent/Dockerfile.services build/workload-microvm/Dockerfile.services
	podman build --arch arm64 --tag hako-workload-agent:local-base -f build/workload-microvm/Dockerfile build/workload-microvm
	podman build --arch arm64 --build-arg HAKO_WORKLOAD_BASE_IMAGE=hako-workload-agent:local-base --build-arg NERDCTL_FULL_VERSION="$(HAKO_NERDCTL_FULL_VERSION)" --build-arg NERDCTL_FULL_SHA256="$(HAKO_NERDCTL_FULL_SHA256)" --build-arg CNI_PLUGINS_VERSION="$(HAKO_CNI_PLUGINS_VERSION)" --build-arg SLIRP4NETNS_VERSION="$(HAKO_SLIRP4NETNS_VERSION)" --build-arg SLIRP4NETNS_SHA256="$(HAKO_SLIRP4NETNS_SHA256)" --tag hako-workload-agent:services-local -f build/workload-microvm/Dockerfile.services build/workload-microvm

# Builds a distinct root-owned Workspace init image with a separate unprivileged shell Agent. Architecture-matched EFS RPM, nerdctl-full, slirp4netns and code-server inputs are pinned.
HAKO_WORKSPACE_IMAGE_ARCH ?= arm64
HAKO_WORKSPACE_IMAGE_TAG ?= hako-workspace-init:local
ifeq ($(HAKO_WORKSPACE_IMAGE_ARCH),amd64)
HAKO_WORKSPACE_BASE_IMAGE ?= public.ecr.aws/lambda/microvms@sha256:10fe67044ef13e1795b8ae8281fdaa3fe2efb26ecd101e9223beb169c4c373da
else
HAKO_WORKSPACE_BASE_IMAGE ?= public.ecr.aws/lambda/microvms@sha256:b6a34591182b4a1969f57eb24c812582d0c52dc2e40b75b73c0df371bb7aef82
endif

build-workspace-microvm-image: check-workspace-image-build-host
	@test "$(HAKO_WORKSPACE_SERVICES_ENABLED)" = true -o "$(HAKO_WORKSPACE_SERVICES_ENABLED)" = false || (echo "set HAKO_WORKSPACE_SERVICES_ENABLED to true or false" >&2; exit 1)
	@test "$(HAKO_WORKSPACE_AGENT_VSOCK_ENABLED)" = true -o "$(HAKO_WORKSPACE_AGENT_VSOCK_ENABLED)" = false || (echo "set HAKO_WORKSPACE_AGENT_VSOCK_ENABLED to true or false" >&2; exit 1)
	@test -n "$(HAKO_EFS_UTILS_RPM_VERSION)" || (echo "set HAKO_EFS_UTILS_RPM_VERSION to the pinned amazon-efs-utils RPM version" >&2; exit 1)
	@test -n "$(HAKO_OPENSSH_RPM_VERSION)" || (echo "set HAKO_OPENSSH_RPM_VERSION to the pinned OpenSSH RPM version" >&2; exit 1)
	@test -n "$(HAKO_NERDCTL_FULL_ARCHIVE)" || (echo "set HAKO_NERDCTL_FULL_ARCHIVE to a reviewed nerdctl-full Linux $(HAKO_WORKSPACE_IMAGE_ARCH) archive" >&2; exit 1)
	@test -n "$(HAKO_NERDCTL_FULL_VERSION)" || (echo "set HAKO_NERDCTL_FULL_VERSION to the exact nerdctl release" >&2; exit 1)
	@test -n "$(HAKO_NERDCTL_FULL_SHA256)" || (echo "set HAKO_NERDCTL_FULL_SHA256 to the reviewed archive digest" >&2; exit 1)
	@test -n "$(HAKO_CNI_PLUGINS_VERSION)" || (echo "set HAKO_CNI_PLUGINS_VERSION to the exact CNI plugins version in nerdctl-full" >&2; exit 1)
	@test -n "$(HAKO_SLIRP4NETNS_BINARY)" || (echo "set HAKO_SLIRP4NETNS_BINARY to a reviewed Linux $(HAKO_WORKSPACE_IMAGE_ARCH) slirp4netns binary" >&2; exit 1)
	@test -n "$(HAKO_SLIRP4NETNS_VERSION)" || (echo "set HAKO_SLIRP4NETNS_VERSION to the exact slirp4netns release" >&2; exit 1)
	@test -n "$(HAKO_SLIRP4NETNS_SHA256)" || (echo "set HAKO_SLIRP4NETNS_SHA256 to the reviewed binary digest" >&2; exit 1)
	@test -s "$(HAKO_SLIRP4NETNS_BINARY)" || (echo "slirp4netns binary is missing or empty" >&2; exit 1)
	@test -n "$(HAKO_CODE_SERVER_VERSION)" || (echo "set HAKO_CODE_SERVER_VERSION to the pinned code-server version" >&2; exit 1)
	@test -n "$(HAKO_CODE_SERVER_SHA256)" || (echo "set HAKO_CODE_SERVER_SHA256 to the pinned $(HAKO_WORKSPACE_IMAGE_ARCH) archive SHA-256" >&2; exit 1)
	@test -n "$(HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE)" || (echo "set HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE to a public-key JSON file" >&2; exit 1)
	@test -s "$(HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE)" || (echo "Workspace Agent public-key JSON file is missing or empty" >&2; exit 1)
	$(MAKE) validate-workspace-agent-keys HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE="$(HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE)" HAKO_PREVIEW_TICKET_KEY_ID="$(HAKO_PREVIEW_TICKET_KEY_ID)"
	@test -s "$(HAKO_NERDCTL_FULL_ARCHIVE)" || (echo "nerdctl-full archive is missing or empty" >&2; exit 1)
	@set -e; archive_entries=$$(tar -tzf "$(HAKO_NERDCTL_FULL_ARCHIVE)"); \
	for required in bin/nerdctl bin/containerd bin/containerd-rootless.sh bin/rootlesskit libexec/cni/firewall share/doc/nerdctl-full/README.md; do \
		printf '%s\n' "$$archive_entries" | grep -Fxq "$$required" || { echo "nerdctl-full archive is missing $$required" >&2; exit 1; }; \
	done
	mkdir -p build/workspace-init
	GOOS=linux GOARCH=$(HAKO_WORKSPACE_IMAGE_ARCH) CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o build/workspace-init/hako-workspace-init ./cmd/hako-workspace-init
	GOOS=linux GOARCH=$(HAKO_WORKSPACE_IMAGE_ARCH) CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o build/workspace-init/hako-workspace-agent ./cmd/hako-workspace-agent
	GOOS=linux GOARCH=$(HAKO_WORKSPACE_IMAGE_ARCH) CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o build/workspace-init/hako-local-workspace-guest-init ./cmd/hako-local-workspace-guest-init
	@if [ "$$(realpath -m "$(HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE)")" != "$$(realpath -m build/workspace-init/trusted-keys.json)" ]; then install -m 0644 "$(HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE)" build/workspace-init/trusted-keys.json; fi
	install -m 0644 "$(HAKO_NERDCTL_FULL_ARCHIVE)" build/workspace-init/nerdctl-full-linux-$(HAKO_WORKSPACE_IMAGE_ARCH).tar.gz
	install -m 0644 "$(HAKO_SLIRP4NETNS_BINARY)" build/workspace-init/slirp4netns-$(HAKO_WORKSPACE_IMAGE_ARCH)
	install -m 0644 images/workspace-init/Dockerfile build/workspace-init/Dockerfile
	podman build --arch $(HAKO_WORKSPACE_IMAGE_ARCH) --build-arg HAKO_IMAGE_ARCH="$(HAKO_WORKSPACE_IMAGE_ARCH)" --build-arg HAKO_WORKSPACE_BASE_IMAGE="$(HAKO_WORKSPACE_BASE_IMAGE)" --build-arg EFS_UTILS_RPM_VERSION="$(HAKO_EFS_UTILS_RPM_VERSION)" --build-arg OPENSSH_RPM_VERSION="$(HAKO_OPENSSH_RPM_VERSION)" --build-arg NERDCTL_FULL_VERSION="$(HAKO_NERDCTL_FULL_VERSION)" --build-arg NERDCTL_FULL_SHA256="$(HAKO_NERDCTL_FULL_SHA256)" --build-arg CNI_PLUGINS_VERSION="$(HAKO_CNI_PLUGINS_VERSION)" --build-arg SLIRP4NETNS_VERSION="$(HAKO_SLIRP4NETNS_VERSION)" --build-arg SLIRP4NETNS_SHA256="$(HAKO_SLIRP4NETNS_SHA256)" --build-arg CODE_SERVER_VERSION="$(HAKO_CODE_SERVER_VERSION)" --build-arg CODE_SERVER_SHA256="$(HAKO_CODE_SERVER_SHA256)" --build-arg HAKO_WORKSPACE_SERVICES_ENABLED="$(HAKO_WORKSPACE_SERVICES_ENABLED)" --build-arg HAKO_WORKSPACE_AGENT_VSOCK_ENABLED="$(HAKO_WORKSPACE_AGENT_VSOCK_ENABLED)" --tag "$(HAKO_WORKSPACE_IMAGE_TAG)" -f build/workspace-init/Dockerfile build/workspace-init

check-workspace-image-build-host:
	@set -e; host_arch=$$(uname -m); case "$$host_arch" in x86_64) host_arch=amd64 ;; aarch64|arm64) host_arch=arm64 ;; esac; \
	case "$(HAKO_WORKSPACE_IMAGE_ARCH)" in amd64|arm64) ;; *) echo "HAKO_WORKSPACE_IMAGE_ARCH must be amd64 or arm64" >&2; exit 1 ;; esac; \
	if [ "$$host_arch" != "$(HAKO_WORKSPACE_IMAGE_ARCH)" ]; then \
	  handler=qemu-$(HAKO_WORKSPACE_IMAGE_ARCH); \
	  for path in "/proc/sys/fs/binfmt_misc/$$handler" "/proc/sys/fs/binfmt_misc/$${handler}-static"; do \
	    if [ -r "$$path" ] && grep -q '^enabled$$' "$$path"; then exit 0; fi; \
	  done; echo "cross-architecture Workspace OCI builds require enabled $$handler binfmt_misc" >&2; exit 1; \
	fi

# Builds an ARM64 ECS/Fargate image locally; publishing to ECR is a separate authorized step.
build-github-checks-consumer-image: check-arm64-podman-build
	podman build --arch arm64 --tag hako-checks-consumer:local -f cmd/hako-checks-consumer/Dockerfile .

# Cross-compiles the non-root ARM64 GitHub Credential Broker image; no target-architecture build steps execute.
build-github-broker-image:
	podman build --arch arm64 --tag hako-github-broker:local -f cmd/hako-github-broker/Dockerfile .

# Builds a non-root ARM64 Gateway image locally; publication/deployment remain release operations.
build-workspace-gateway-image: check-arm64-podman-build
	podman build --arch arm64 --tag hako-workspace-gateway:local -f cmd/hako-workspace-gateway/Dockerfile .

terraform-fmt:
	$(TERRAFORM) fmt -check -recursive infra/terraform

terraform-validate:
	@set -e; \
	for root in infra/terraform/environments/dev/control-plane infra/terraform/environments/dev/resource-plane; do \
		$(TERRAFORM) -chdir=$$root init -backend=false -input=false; \
		$(TERRAFORM) -chdir=$$root validate; \
	done

terraform-test:
	$(TERRAFORM) -chdir=infra/terraform/modules/control-plane init -backend=false -input=false
	$(TERRAFORM) -chdir=infra/terraform/modules/control-plane test
	$(TERRAFORM) -chdir=infra/terraform/modules/resource-plane init -backend=false -input=false
	$(TERRAFORM) -chdir=infra/terraform/modules/resource-plane test
	$(TERRAFORM) -chdir=infra/terraform/modules/workspace-storage init -backend=false -input=false
	$(TERRAFORM) -chdir=infra/terraform/modules/workspace-storage test
	$(TERRAFORM) -chdir=infra/terraform/modules/github-broker-tenant-iam init -backend=false -input=false
	$(TERRAFORM) -chdir=infra/terraform/modules/github-broker-tenant-iam test
	$(TERRAFORM) -chdir=infra/terraform/modules/tenant-model-credential-reader-role init -backend=false -input=false
	$(TERRAFORM) -chdir=infra/terraform/modules/tenant-model-credential-reader-role test
