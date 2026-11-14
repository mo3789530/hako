# Task 92: Coding Agent Harness and Model Policy

Status: Planned. Priority: P3. Parent: Task 90. Depends on Tasks 53 and 91.

## Goal

Define how HakoCodingAgent selects its Python harness and model, and make an
informed decision on additional harnesses without conflating model choice,
execution backend and authorization.

## Decisions and work

1. Define a versioned harness contract for task and source inputs, supported
   tools, progress events, cancellation, resource/time/token limits, terminal
   outcomes and bounded `agent_changes` output. Treat harness output as
   untrusted; independent Hako verification and Broker publication remain
   mandatory. Retain the current Python Bedrock implementation as a baseline.
2. Evaluate Claude Agent SDK, Codex and other candidates as appropriate against
   AgentCore and, if selected by Task 91, Lambda MicroVM. Compare Bedrock/model
   support, credentials, ARM64 dependencies, tool and shell controls, process
   lifecycle, licensing and operational cost. Select and document a first
   optional adapter only if its boundary can meet the contract.
3. Specify trusted model/provider/region selection, per-tenant policy, maximum
   turns, token and spend budgets, timeout and model-unavailable behavior.
   Distinguish enforceable ceilings from billing notifications.
4. Specify how prompts, repository content and third-party instructions are
   handled as untrusted, how secrets are prevented from leaking to tools and
   logs, and how tool permissions are enforced outside model instructions.
5. Add adapter contract tests with synthetic clients for allowed/denied tools,
   error handling, cancellation, limits and malformed results. No provider calls
   or extra harness activation are required for local tests.

## Acceptance criteria

- ADR records the first supported harness/model combinations and explicit
  deferrals, including supported runtime and credential requirements.
- Runtime cannot choose an unapproved model or harness from task/repository
  input, and tests demonstrate applicable authorization and budget checks.
- Live provider and target-image behavior are separately verified before enablement.
