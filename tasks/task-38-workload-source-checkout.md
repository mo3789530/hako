# Task 38: Secure Immutable Repository Checkout

Status: Done (checkout package, docs, and acceptance tests; 2026-10-10). Priority: P0.

## Goal

Provide a worker-side source provider that checks out only the Tenant-authorized GitHub Repository and exact immutable commit stored in `workload_runs`.

## Work

- Define a narrow `RepositoryCheckout` interface consumed by the worker, with a temporary checkout path and deterministic cleanup.
- Validate Run kind, active Tenant Installation binding, repository ID, commit SHA, and desired state before requesting credentials.
- Use Task 37's short-lived App Installation token only for fetching; prevent tokens from appearing in git remote configuration, process args, environment exported to the job container, logs, or artifacts.
- Fetch the exact commit by repository identity and SHA. Verify the resulting `HEAD` equals the persisted lowercase SHA before returning the path.
- Use an isolated temporary directory with safe permissions; clean it on success, error, cancellation, and timeout.
- Define bounded fetch size/time and prevent submodule/LFS or redirect behavior from widening credential/network scope unless explicitly approved by policy.

## Acceptance criteria

- Fake GitHub/Git tests cover correct commit, mutable ref mismatch, wrong repository, unavailable commit, suspended Installation, credential redaction, timeout, cleanup on every exit path, and path traversal defenses.
- A test proves user-controlled ref metadata never selects the checked-out revision.
- No clone is performed on the API process; source checkout occurs only in the isolated worker execution boundary selected by Task 36.
- Document supported repository size/features and any intentionally rejected clone behavior.
- Tests for the new Repository Checkout implementation achieve at least **80% statement coverage**. Report the measured percentage from the checkout implementation package, separately from aggregate coverage of the broader `workloadjobs` package.

## Progress (2026-10-10)

- Added `internal/repositorycheckout` with canonical Tenant repository authorization before credential access, immutable SHA fetch, private temporary storage, FD-based askpass, disabled redirects/hooks, and rejected submodule/LFS sources.
- Fake local Git repository tests cover exact SHA despite mutable ref metadata, unavailable commits, authorization denial, suspended installations, token redaction, timeout, cleanup, size limit, and path traversal.
- `go test -cover ./internal/repositorycheckout` reports 83.6% statement coverage. Handler integration remains Task 39; worker deployment remains Task 40.
