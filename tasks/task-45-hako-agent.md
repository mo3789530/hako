# Task 45: Workspace Agent and Secret References

Status: Blocked on Task 43. Priority: P1.

## Goal

Provide the in-MicroVM `hako-agent` protocol needed for lifecycle reporting and developer commands, without exposing provider credentials or long-lived access tickets.

## Work

- Define an authenticated, versioned agent control protocol and heartbeat/lease behavior.
- Implement bounded exec, shell/PTY, process lifecycle, file transfer, service control, and cancellation with explicit resource and output limits.
- Use Secret Manager references; fetch secrets only into the target process at runtime and redact outputs. Never persist plaintext into DSQL, image, EFS source tree, or logs.
- Add protocol compatibility/version handling, heartbeat loss behavior, and audit correlation IDs.

## Acceptance criteria

- Unit/integration tests cover authorization, PTY lifecycle, disconnect/reconnect, process cleanup, oversized requests/output, malformed frames, and secret redaction.
- Agent listens only on private Runtime interfaces; no public SSH or service/database ports are introduced.
- Document image build, update/rollback, protocol compatibility, and secret rotation.
