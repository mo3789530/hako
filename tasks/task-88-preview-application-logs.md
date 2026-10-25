# Task 88: Preview Application Log Tail

Status: Implemented locally. Preview Guest stdout/stderr is available as a
bounded in-memory tail through a dedicated read-only Gateway capability and
`hako github repository previews logs`. The matching ARM64 image and deployed
MicroVM/Gateway path remain part of Task 54's AWS validation gate.
Priority: P2. Depends on: Task 54 Preview runtime and Gateway session flow.

## Scope

- Keep at most the latest 256 KiB of Preview application stdout/stderr in the
  Guest process memory; do not persist it in DSQL, S3, or Control Plane logs.
- Add a dedicated `preview-logs` Session permission. Only the Preview Session
  API may issue it, and Preview Sessions must reject shell, exec, SSH, and
  general file-transfer permissions.
- Expose a read-only Gateway route that uses one fixed Guest operation and
  fixed path. A caller with ordinary `file-transfer` permission cannot use it.
- Add `hako github repository previews logs` to create a narrowly scoped
  Session, read the bounded response, print it to stdout, and revoke the
  Session.
- Document retention, permissions, CLI usage, and deployment limitations.

## Acceptance criteria

- Unit and local integration tests cover bounded Guest output, permission
  issuance/rejection, fixed Gateway operation/path, read-only streaming, CLI
  output, and Session revocation.
- Ordinary Workspace Session creation cannot mint `preview-logs`.
- Guest logs are not written to persistent storage and disappear when the
  MicroVM is replaced.
- `docs/preview-lifecycle.md`, `docs/workspace-gateway.md`, and README describe
  the feature and distinguish local verification from AWS runtime validation.

## Verification

The focused Go unit/client/API tests and PostgreSQL API authorization tests
passed during implementation. The local PostgreSQL container remained running.
AWS deployment, matching ARM64 image execution, and live Lambda MicroVM log
retrieval were not performed.
