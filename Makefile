.PHONY: fmt lint test test-integration test-integration-local test-webhook-lifecycle-integration test-tenant-app-proof-integration test-tenant-github-apps-integration test-actions-runner-lifecycle-integration test-software-factory-e2e test-native-agent-e2e test-api-lambda-image-smoke test-api-lambda-rie-smoke test-aws-e2e-guard aws-e2e-preflight test-workload-runtime test-workload-runtime-integration test-job-services-podman test-workspace-service-runtime-integration test-workspace-podman-network test-artifact-retention-integration test-workspace-idle-lifecycle-integration test-preview-expiry-integration test-coverage integration-db-up migrate build-api-lambda build-api-lambda-image build-dispatcher-lambda build-webhook-processor-lambda build-resource-controller-lambda build-workspace-service-controller-lambda build-health-reporter-lambda build-workspace-idle-reconciler-lambda build-preview-expiry-lambda build-preview-runtime-dispatch-lambda build-preview-result-consumer-lambda build-workload-controller-lambda build-actions-runner-lambda build-actions-runner-microvm-image build-workload-microvm-artifact verify-workload-microvm-artifact build-workload-microvm-services-image build-workspace-microvm-image build-github-checks-consumer-image build-github-broker-image build-workspace-gateway-image check-arm64-podman-build terraform-fmt terraform-validate terraform-test

TERRAFORM ?= terraform

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

# Composes the signed Issue command inbox, persisted bounded Agent task, and local Native Agent proposal executor.
test-native-agent-e2e:
	HAKO_TEST_DATABASE_URL="$${HAKO_TEST_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" go test -tags=integration ./internal/workloadjobs -run '^TestIssueCommandToNativeAgentProposalUsesPersistedBoundedContext$$' -count=1

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
	podman build --arch amd64 --build-arg TARGETARCH=amd64 -f cmd/hako-api/Dockerfile -t hako-api:smoke .
	HAKO_DATABASE_URL="$${HAKO_DATABASE_URL:-postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable}" HAKO_API_SMOKE_IMAGE=hako-api:smoke ./scripts/test-api-lambda-image-smoke.sh

# Uses AWS's Runtime Interface Emulator to exercise the image's Lambda Web Adapter with API Gateway v2 events.
test-api-lambda-rie-smoke:
	podman build --arch amd64 --build-arg TARGETARCH=amd64 -f cmd/hako-api/Dockerfile -t hako-api:rie-smoke .
	podman build --arch amd64 -f cmd/hako-api/Dockerfile.rie-smoke -t hako-api:rie-local .
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

# Builds a distinct root-owned Workspace init image with a separate unprivileged shell Agent. EFS RPM, nerdctl-full artifact, and public verification keys are pinned inputs.
build-workspace-microvm-image: check-arm64-podman-build
	@test "$(HAKO_WORKSPACE_SERVICES_ENABLED)" = true -o "$(HAKO_WORKSPACE_SERVICES_ENABLED)" = false || (echo "set HAKO_WORKSPACE_SERVICES_ENABLED to true or false" >&2; exit 1)
	@test -n "$(HAKO_EFS_UTILS_RPM_VERSION)" || (echo "set HAKO_EFS_UTILS_RPM_VERSION to the pinned amazon-efs-utils RPM version" >&2; exit 1)
	@test -n "$(HAKO_OPENSSH_RPM_VERSION)" || (echo "set HAKO_OPENSSH_RPM_VERSION to the pinned OpenSSH RPM version" >&2; exit 1)
	@test -n "$(HAKO_NERDCTL_FULL_ARCHIVE)" || (echo "set HAKO_NERDCTL_FULL_ARCHIVE to a reviewed nerdctl-full Linux ARM64 archive" >&2; exit 1)
	@test -n "$(HAKO_NERDCTL_FULL_VERSION)" || (echo "set HAKO_NERDCTL_FULL_VERSION to the exact nerdctl release" >&2; exit 1)
	@test -n "$(HAKO_NERDCTL_FULL_SHA256)" || (echo "set HAKO_NERDCTL_FULL_SHA256 to the reviewed archive digest" >&2; exit 1)
	@test -n "$(HAKO_CNI_PLUGINS_VERSION)" || (echo "set HAKO_CNI_PLUGINS_VERSION to the exact CNI plugins version in nerdctl-full" >&2; exit 1)
	@test -n "$(HAKO_SLIRP4NETNS_BINARY)" || (echo "set HAKO_SLIRP4NETNS_BINARY to a reviewed Linux ARM64 slirp4netns binary" >&2; exit 1)
	@test -n "$(HAKO_SLIRP4NETNS_VERSION)" || (echo "set HAKO_SLIRP4NETNS_VERSION to the exact slirp4netns release" >&2; exit 1)
	@test -n "$(HAKO_SLIRP4NETNS_SHA256)" || (echo "set HAKO_SLIRP4NETNS_SHA256 to the reviewed binary digest" >&2; exit 1)
	@test -s "$(HAKO_SLIRP4NETNS_BINARY)" || (echo "slirp4netns binary is missing or empty" >&2; exit 1)
	@test -n "$(HAKO_CODE_SERVER_VERSION)" || (echo "set HAKO_CODE_SERVER_VERSION to the pinned code-server version" >&2; exit 1)
	@test -n "$(HAKO_CODE_SERVER_SHA256)" || (echo "set HAKO_CODE_SERVER_SHA256 to the pinned ARM64 archive SHA-256" >&2; exit 1)
	@test -n "$(HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE)" || (echo "set HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE to a public-key JSON file" >&2; exit 1)
	@test -s "$(HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE)" || (echo "Workspace Agent public-key JSON file is missing or empty" >&2; exit 1)
	$(MAKE) validate-workspace-agent-keys HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE="$(HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE)" HAKO_PREVIEW_TICKET_KEY_ID="$(HAKO_PREVIEW_TICKET_KEY_ID)"
	@test -s "$(HAKO_NERDCTL_FULL_ARCHIVE)" || (echo "nerdctl-full archive is missing or empty" >&2; exit 1)
	@set -e; archive_entries=$$(tar -tzf "$(HAKO_NERDCTL_FULL_ARCHIVE)"); \
	for required in bin/nerdctl bin/containerd bin/containerd-rootless.sh bin/rootlesskit libexec/cni/firewall share/doc/nerdctl-full/README.md; do \
		printf '%s\n' "$$archive_entries" | grep -Fxq "$$required" || { echo "nerdctl-full archive is missing $$required" >&2; exit 1; }; \
	done
	mkdir -p build/workspace-init
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o build/workspace-init/hako-workspace-init ./cmd/hako-workspace-init
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o build/workspace-init/hako-workspace-agent ./cmd/hako-workspace-agent
	install -m 0644 "$(HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE)" build/workspace-init/trusted-keys.json
	install -m 0644 "$(HAKO_NERDCTL_FULL_ARCHIVE)" build/workspace-init/nerdctl-full-linux-arm64.tar.gz
	install -m 0644 "$(HAKO_SLIRP4NETNS_BINARY)" build/workspace-init/slirp4netns-aarch64
	install -m 0644 images/workspace-init/Dockerfile build/workspace-init/Dockerfile
	podman build --arch arm64 --build-arg EFS_UTILS_RPM_VERSION="$(HAKO_EFS_UTILS_RPM_VERSION)" --build-arg OPENSSH_RPM_VERSION="$(HAKO_OPENSSH_RPM_VERSION)" --build-arg NERDCTL_FULL_VERSION="$(HAKO_NERDCTL_FULL_VERSION)" --build-arg NERDCTL_FULL_SHA256="$(HAKO_NERDCTL_FULL_SHA256)" --build-arg CNI_PLUGINS_VERSION="$(HAKO_CNI_PLUGINS_VERSION)" --build-arg SLIRP4NETNS_VERSION="$(HAKO_SLIRP4NETNS_VERSION)" --build-arg SLIRP4NETNS_SHA256="$(HAKO_SLIRP4NETNS_SHA256)" --build-arg CODE_SERVER_VERSION="$(HAKO_CODE_SERVER_VERSION)" --build-arg CODE_SERVER_SHA256="$(HAKO_CODE_SERVER_SHA256)" --build-arg HAKO_WORKSPACE_SERVICES_ENABLED="$(HAKO_WORKSPACE_SERVICES_ENABLED)" --tag hako-workspace-init:local -f build/workspace-init/Dockerfile build/workspace-init

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
