# Software Factory AWS Smoke Test Runbook

This runbook defines the approval-gated deployed smoke test for Task 42. It is
separate from CI: `make test-software-factory-e2e` uses local PostgreSQL,
Podman, and fake AWS/GitHub providers and never needs AWS credentials.

No smoke run, infrastructure apply, GitHub App change, or real Secret read is
authorized by this document. Each requires explicit approval for the named
accounts, Region, repository, and time window.

## Account and environment boundary

- Use a dedicated Hako staging Control Plane account and a dedicated staging
  Resource Plane account in a non-production AWS Organization OU. Never run
  against the Organization Management account, a developer sandbox shared with
  unrelated work, or production.
- Keep Control Plane and Resource Plane in separate accounts. The smoke must
  use the Lambda MicroVM runtime and an explicitly approved VPC connector;
  EC2 and Fargate are not alternate workload targets.
- Use one dedicated, disposable private GitHub repository and a dedicated
  GitHub App installation with the minimum permissions required by the smoke.
  Do not install it on a production organization or repository.
- Before any deployment, record and independently verify the Control Plane
  account ID, Resource Plane account ID, Region, VPC/subnets/security groups,
  image digest, connector ARN, artifact bucket/prefix, queue/DLQ ARNs, and
  GitHub repository ID. The two AWS account IDs must differ and neither may be
  the Organization Management account.
- The operator must compare `aws sts get-caller-identity` results for both
  account roles with the approved change record. A mismatch, absent identity,
  unexpected Region, or unrecognized Hako resource tag is a hard stop.

## Required approvals

Obtain and record all of the following before deployment or execution:

1. A change approval naming both account IDs, Region, deployment revision,
   Resource Plane, GitHub repository, expected spend limit, and test window.
2. Security approval for the Lambda MicroVM image, VPC connector and egress
   allowlist, IAM policies, short-lived GitHub App permissions, log retention,
   and cleanup procedure.
3. Resource Plane operator approval for the exact Terraform plan and the
   specific runtime/image inputs. Keep Workload creation disabled until the
   reviewed plan and prerequisites are in place.
4. Repository owner approval for installing the dedicated GitHub App and
   submitting one test Pull Request or commit.

The change record must identify who can stop the run and who verifies cleanup.
Approval of this runbook is not approval to apply Terraform or access real
Secrets.

## Smoke scope

Run one bounded, non-privileged Job against the disposable repository and an
immutable commit. The expected path is:

```text
signed GitHub event
  -> Hako Run and Check Outbox
  -> Resource Plane queue
  -> one Lambda MicroVM attempt
  -> fixed test command
  -> redacted result and Check
  -> confirmed MicroVM termination
```

Use a synthetic test fixture that does not require production credentials,
cloud roles, or access to customer data. Confirm the final Run and GitHub Check
conclusions, attempt-scoped redacted logs/artifacts, correlation IDs, and
provider-confirmed MicroVM termination. Verify the queue, journal, and artifact
resources are scoped to the approved Resource Plane and exact test prefix.

Do not use a test-only success result as evidence of egress isolation, kernel
cgroup enforcement, or image security. Record those checks separately with the
runtime evidence collected from the deployed Lambda MicroVM.

## Resource ownership and cleanup

Every smoke-created resource must carry the established Hako ownership tags,
including Tenant, Run/attempt or Operation ID where supported. Capture a
pre-run inventory for the exact test prefix and resource identifiers. Cleanup
may target only resources listed in that run's inventory and verified to have
the matching ownership tags. Never use account-wide, Region-wide, or
tag-wildcard deletion.

After the run:

1. Stop further Workload admission for the smoke Repository.
2. Confirm the Worker has reported terminal state and the Lambda MicroVM is
   terminated; retry through the normal cleanup controller if needed.
3. Remove only the exact smoke-owned temporary resources. Retain artifacts and
   logs only for the approved retention period, then remove the exact
   attempt-scoped objects.
4. Confirm no smoke-owned MicroVM, queue message, journal lease, temporary
   credential, or temporary GitHub branch remains. Preserve required audit and
   deployment records.
5. If ownership is ambiguous or cleanup is not confirmed, stop deletion, keep
   admission disabled, and escalate to the named Resource Plane operator.

Attach a cleanup report to the change record with:

```text
change_id:
operator:
verified_by:
control_account_id:
resource_account_id:
region:
resource_plane_id:
workload_run_id:
attempt_id:
microvm_id:
github_repository_id:
created_resources: []
deleted_resources: []
retained_audit_artifacts: []
cleanup_failures: []
admission_disabled: true|false
completion_time_utc:
```

Do not include access tokens, Secret values, private keys, or credential-bearing
payloads in the report. A smoke is not complete until cleanup is confirmed by
the Resource Plane and independently reviewed.

## Current status

The local PostgreSQL/Podman/fake-provider E2Es are available through
`make test-software-factory-e2e`. This deployed runbook is a prerequisite only;
no dedicated AWS accounts, production certificates, real Secrets, deployment,
or live Lambda MicroVM smoke have been verified by the local test suite.
