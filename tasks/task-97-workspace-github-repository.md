# Task 97: Workspace GitHub Repository Option

Status: Local implementation/PostgreSQL acceptance complete. Real KVM Guest/Gateway first checkout, readback, identical retry, edit-preservation, and edited-checkout persistence across cold suspend/resume now pass using a current local initrd. Only deployed AWS runtime acceptance remains. The CLI accepts `--repository owner/repo`; the create API resolves only through the same Tenant's active GitHub App registry. A transactional checkout record persists Installation/repository/default-branch identity with `pending` state, and create, idempotent replay, list, and detail models expose it; `hako get` displays the selection. Resource Plane commands carry and validate the Installation/repository pin. PostgreSQL API integration passes against local `hako-dev-postgres`, covering Tenant authorization, idempotency, detail state, immutable SHA pin replay/conflict, signed failed→ready checkout retry, and pin retention after suspend/resume. The Broker exposes a Resource Plane mTLS checkout endpoint that accepts only Tenant/Workspace identity, resolves the authorized repository pin from PostgreSQL, mints a single-repository token, resolves and persists the initial default-branch SHA, and rechecks authorization after minting. Unit/HTTP tests and a PostgreSQL resolver integration test pass locally. A signed Resource Plane result API and HMAC client persist checkout state. The Guest accepts a bounded credential-free Git checkout archive with `.git` metadata, verifies its SHA-256 and detached HEAD pin, installs atomically, accepts exact-content retries, and preserves an existing locally edited checkout. Resource Controller composes Broker checkout, credentialed fetch, archive transfer, Guest install and signed result reporting; ready-state retries reuse the immutable commit pin. A composed local runtime test verifies checkout runs before first Ensure and is not replayed through Suspend/Resume after a Workspace edit; the Guest filesystem tests separately verify retry behavior preserves edits. Terraform has an optional checkout configuration and grants exactly the five configured Secrets plus MicroVM auth-token actions. The configured mock plan now asserts the exact five Secret ARNs (not only their count); `tofu fmt -check` and `tofu test -filter=workload-controller.tftest.hcl` pass with 11 tests. `make test-integration-local` and `tofu validate` pass. Remaining runtime acceptance requires the configured Control Plane/Broker endpoints and deployed Gateway/MicroVM; no AWS service was contacted. Depends on Tasks 38, 43, 47 and 66.

## Goal

Let a developer select a GitHub repository when creating an interactive
Workspace and find its checkout under `/workspace` on first connection.

## Work

- Add an optional `--repository <owner>/<repo>` to `hako create` and an
  equivalent field to the Workspace create API. Resolve the selection through
  the Tenant's active GitHub App binding and repository registry. Persist the
  repository identity and the resolved initial commit. Include the selection
  in idempotency checks and expose the repository and checkout state in
  Workspace details.
- Reuse the Tenant-scoped credential broker and immutable checkout logic
  where applicable. Scope checkout authorization to read access to the single
  repository. Keep installation tokens out of the Workspace and its persisted
  Git configuration. Leave subsequent interactive Git authentication and
  updates to the developer.
- Integrate first checkout with Workspace provisioning. Install it under
  `/workspace` as the Workspace user and retain an authenticated-token-free
  Git origin. Stage the install before publishing it, report checkout failures,
  and make retries safe against duplicate delivery and runtime replacement.
  Preserve existing user files and avoid resetting the checkout on resume.

## Acceptance criteria

- Workspace creation without the option and existing Workspaces retain their
  existing behavior. An authorized public or private repository is checked
  out at the recorded commit and visible in the shell and VS Code.
- Missing, inaccessible, and foreign-Tenant repositories are rejected.
  Installation tokens are absent from Guest state, responses, and logs.
- Tests cover CLI/API behavior, authorization, duplicate requests, failed
  checkout followed by retry, and preservation of local edits on resume.
  Verify first checkout against the deployed Workspace runtime separately.

2026-11-23 KVM acceptance: Task 98's opt-in real-Firecracker test drives `hako
exec` through the local API session, signed Gateway stream, and Guest, then
installs a pinned Git repository through the signed file-transfer stream. The
first attempt exposed that the OCI rootfs had `/tmp` mode 0755, preventing UID
100000 from creating the verified temporary archive. The initrd packager now
normalizes `/tmp` to sticky mode 01777, with a newc mode regression test. A
current Guest initrd rebuilt from the existing extracted OCI rootfs plus the
current Agent and Guest-init binaries passed first install, Guest readback,
identical retry, and rejection of a retry after a local edit; readback confirmed
the edit remained. The same real Firecracker allocation was then cold-suspended
and resumed from its Workspace disk, and the edited checkout remained readable. `make test-local-workspace-cli-firecracker` passes on real KVM.
Only the deployed AWS runtime remains unaccepted; no AWS or GitHub service was contacted.
