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
