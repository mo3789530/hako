# Task 41: GitHub Checks API Results

Status: Blocked on Task 37; depends on Task 39 for real execution results. Priority: P0.

## Goal

Publish Hako Job status to the exact GitHub Repository and immutable commit associated with a Workload Run.

## Work

- Add `checks:write` only when this feature is enabled; keep Installation token scopes least-privilege.
- Persist Check Run ID and the exact repository/commit association as metadata so retries update the same Check rather than create duplicates.
- Report queued/in-progress/completed and bounded summaries for passed, failed, timed out, cancelled, and infrastructure-error outcomes.
- Validate annotation paths/line numbers and cap summary/annotation counts and bytes. Never publish secrets or raw unredacted logs.
- Treat GitHub API rate limits and temporary errors as retryable without re-running the Job.

## Acceptance criteria

- Fake GitHub API tests cover idempotent create/update, commit mismatch refusal, permission failure, rate limiting, annotations, result mapping, and retry after update failure.
- Database changes use migrations and do not store Installation tokens.
- No Check is attached using a branch name or a Repository ID from an untrusted webhook without Tenant registry validation.
- Docs describe App permission changes and rollback.
