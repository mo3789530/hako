# Task 99: Managed Runtime Routing and Acceptance

Status: Local acceptance complete; deployed staging acceptance remains. Depends on Tasks 45–47, 52, 73–77 and 90–96.

## Current status

The underlying local implementation is tracked by Tasks 52, 73–77, 89 and
90–96. The local targets separate unit acceptance from PostgreSQL-backed lifecycle
acceptance. `make test-managed-runtime-local` covers Actions Runner, Workspace
session/Gateway/runtime, AgentCore preparation, and the Python Coding Agent
core. `make test-managed-runtime-integration` runs the existing fake-provider
Actions/Claude credential and Workspace lifecycle integration tests.

`make test-managed-runtime-local` passed on 2026-11-21: all five Go packages
and all 32 Python Coding Agent tests passed. After starting the existing local
`hako-dev-postgres` container, `make test-managed-runtime-integration` passed
the Actions/Claude credential lifecycle and Workspace idle lifecycle
integration targets; `make test-integration-local` also passed across all Go
packages. These use fake providers and made no AWS or GitHub requests. The
following deployed checks remain to be exercised in the
approved staging environment: actual GitHub Actions and Claude Code Action on
Lambda MicroVM, Coding Agent invocation and cleanup on AgentCore, and
browser/Remote SSH Workspace lifecycle on Lambda MicroVM.

## Goal

Validate the managed deployment routes for GitHub Actions, Claude Code
Action, Hako Coding Agent Runs, and interactive VS Code Workspaces.

| Entry point | Execution target |
| --- | --- |
| Standard GitHub Actions workflow | Ephemeral GitHub Actions runner on Lambda MicroVM |
| GitHub Actions workflow with Claude Code Action | Same runner profile, with model credentials authorized for the approved workflow |
| Hako Coding Agent Run | Amazon Bedrock AgentCore Runtime, with a session per attempt |
| VS Code Workspace | Lambda MicroVM Workspace Agent through the Hako Gateway |

## Work and acceptance criteria

1. Run an allowed standard GitHub Actions workflow in a disposable repository.
   Confirm that one ephemeral runner accepts the matching job, reports its
   final status, and is deregistered and terminated. Repeat with cancellation.
2. Run the approved, pinned Claude Code Action in the same runner profile.
   Verify the model call, redaction, and credential availability only to
   the authorized runner process. Confirm that a fork or an unapproved
   workflow cannot receive the Tenant model credential.
3. Submit a Hako Coding Agent Run at a pinned commit. Verify AgentCore
   invocation, proposal collection, and session cleanup. Test the unavailable
   provider case and verify the Run does not fall back to Lambda. Keep PR
   publication gated on independent verification bound to the proposal digest.
4. Open a disposable Workspace via browser VS Code and Remote SSH. Verify
   Gateway routing and authorization, persistent files after suspend/resume,
   and cleanup after deletion.
5. Cover routing and rejection locally with fake providers. For the
   deployed smoke, record job, Run, session, Workspace and provider IDs,
   terminal results, and provider cleanup evidence in staging.

See [managed routing](../docs/coding-agent-platform.md#managed-runtime-routing)
and the [AWS smoke runbook](../docs/software-factory-aws-smoke.md).
