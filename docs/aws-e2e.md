# AWS integration E2E environment and cleanup

AWS integration E2E tests are not part of the default unit or local PostgreSQL
test suite. Running them is an explicit operation against dedicated,
disposable Control Plane and Resource Plane AWS accounts. Never point an E2E
test at a production, customer, Management, or personal account.

## Preconditions

- Two dedicated AWS accounts exist: one Control Plane account and one Resource
  Plane account. They are not the Organizations Management Account.
- The operator has named CLI profiles for both accounts, a deployment region,
  MFA/SSO-backed access, and permission to inspect caller identity. Do not put
  long-lived AWS keys in the repository, shell history, test output, or CI
  variables.
- Before any plan/apply, verify `aws sts get-caller-identity` for each profile
  and compare the returned account ID with the approved test account record.
  `AWS_PROFILE`/profile selection and `AWS_REGION` must be explicit.
- Cognito test users and the test Tenant contain no real personal or customer
  data. Access tokens are short-lived and must not be logged or saved as
  artifacts.
- A disposable test-only Resource Plane manifest points only at the two test
  accounts/regions. SNS alarm topics, if used, must notify a test destination.
- Set an account budget and resource quotas before provisioning. A test run
  must use a unique run ID and bounded Workspace count/runtime.

## Current scope

The current executable E2E coverage exercises the authenticated CLI lifecycle
with a fake HTTP API, fake Resource Controller, and a PostgreSQL test database;
see [CLI lifecycle E2E](cli-lifecycle-e2e.md). No AWS end-to-end test currently
creates actual Control Plane/Resource Plane infrastructure or connects through
the Workspace Gateway. Gateway, production runtime, and AWS cleanup automation
are incomplete, so do not treat the local E2E as proof of AWS readiness.

An AWS test must remain opt-in and fail closed unless the operator explicitly
selects the test mode and both account IDs match the approved allowlist. It
must never infer “test account” from a profile name alone. No AWS test is
currently wired to CI; CI should continue to run unit/integration tests without
AWS credentials.

## Provisioning and cleanup runbook

1. Record the unique run ID, both account IDs, region, Terraform workspace/state
   locations, expected resource count, and an operator/expiration time. Pass
   the run ID as the `hako:e2e-run-id` Terraform tag in both dev roots; the
   module reserves `hako:managed-by` and `hako:plane`/`hako:resource-plane-id`
   for Hako ownership metadata.
2. Build the API and Fake Controller artifacts, review both Terraform plans,
   and confirm the provider account identity again immediately before each
   apply. Only use the dedicated test accounts. Production AWS Runtime and
   Gateway connection tests remain blocked until those implementations exist.
3. Apply Resource Plane and Control Plane stacks, bootstrap DSQL roles, apply
   migrations, and register the exact test queue URLs/regions. Use a test
   Cognito identity and a Tenant created solely for this run.
4. Run only the tagged AWS integration tests. Capture request/Operation IDs
   and redacted logs; never persist access tokens, IAM session credentials, or
   database authentication tokens.
5. Stop creating new Workspaces. Request delete for every Workspace created
   by the run and wait for terminal Operations. Inspect failures and both
   command/result DLQs before proceeding. Do not blindly replay or discard
   messages.
6. Verify AWS resources by `hako:managed-by`, run ID tag, tenant/workspace IDs,
   and Terraform state. Remove only resources known to belong to this run.
   Preserve diagnostic logs/state until the test owner confirms cleanup.
7. Destroy Resource Plane resources, then disposable Control Plane resources,
   only after data/log retention requirements are satisfied and a human has
   reviewed the exact plan. Aurora DSQL deletion protection is on by default;
   do not disable it for shared environments. For a dedicated disposable
   account, disable it only in a reviewed run-specific change immediately
   before cleanup.
8. Re-check both accounts for resources with the run ID and expected Hako tags,
   empty the test Tenant only after audit review, and record remaining/orphaned
   resources and final cost.

There is no automatic `terraform destroy`, broad tag-based deletion, or AWS
cleanup command in the repository. If any precondition fails, stop before
creating resources and ask the test-account owner to resolve it.

## Implementation status

This document defines environment isolation, opt-in conditions, CI boundary,
and cleanup expectations. The actual AWS lifecycle E2E test, account allowlist
guard, test stack ownership, runtime/Gateway connectivity assertions, and
automated resource cleanup are not implemented yet.
