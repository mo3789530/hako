# Task 49: Isolated AWS Environment and E2E

Status: Blocked on Tasks 43, 44, 46, and 48. Priority: P2.

## Goal

Reproducibly validate the real Workspace lifecycle in a dedicated AWS environment without using the Management Account or production data.

## Work

- Complete Control Plane/Resource Plane account separation and deploy configuration for an explicitly named dev/test account pair.
- Add the remaining API Gateway v2/Lambda Web Adapter image candidate invoke/cutover/rollback smoke tests.
- Add an AWS E2E that creates, connects to, suspends/resumes, and deletes a Workspace, verifying EFS persistence and Gateway authorization.
- Guard account/region/resource tags before any operation; make cleanup idempotent and report resources that could not be deleted.
- Add scheduled CI with dedicated OIDC role and protected environment approval; keep normal PR CI fake-backed.

## Acceptance criteria

- Test refuses Management/production accounts and checks expected account ID, region, and opt-in flag before creation.
- Cleanup covers timeout, failed test, partial create, and manual cancellation; no broad account/resource wildcard deletion.
- Document AWS prerequisites, estimated cost/TTL, rollback, and evidence artifacts.
- This task requires a separate explicit approval for apply/cutover; code completion alone does not authorize deployment.

## Read-only account preflight

Added `make aws-e2e-preflight` (`go run ./cmd/hako-aws-e2e-preflight preflight`)
and `make test-aws-e2e-guard`. The
preflight requires explicit opt-in, two distinct named profiles and account
IDs, an explicit Region/run ID, a Management Account ID, and a production
account denylist. It rejects ambient AWS credential environment variables,
then performs only STS `GetCallerIdentity` for each profile and compares both
caller IDs to the allowlist. Unit tests use mocked lookups and prove no STS
lookup occurs when any configuration, opt-in, or ambient-credential guard
fails. Resource creation/cleanup is not wired to this command; the actual AWS
Workspace E2E remains unimplemented and requires separate approval.

## Local OCI image smoke

`make test-api-lambda-image-smoke` builds the API OCI image for the local
amd64 host with Podman and runs it in HTTP mode against the existing disposable
PostgreSQL database. It verifies public liveness (`/healthz` is 200) and that
the application health endpoint stays authenticated (`/v1/health` is 401
without a token), then removes its uniquely named smoke container. This is a
local image/process and API-auth boundary check. It does not exercise Lambda
Web Adapter's Runtime API event conversion, API Gateway, DSQL IAM, candidate
Lambda invocation, cutover, or rollback; those AWS checks remain gated on an
explicit deployment approval.

`make test-api-lambda-rie-smoke` additionally runs the exact API binary and
Lambda Web Adapter from that image under AWS's Runtime Interface Emulator. It
submits representative HTTP API v2 events and checks the returned proxy
statuses (`/healthz` 200, unauthenticated `/v1/health` 401), JSON bodies,
content type, request-ID header, and response encoding. It is an actual
local extension/event-conversion acceptance; RIE does not reproduce Lambda
orchestration, API Gateway JWT authorization, AWS IAM/network behavior, or a
deployed candidate. Those remain separate approved AWS checks.
