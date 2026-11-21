# Task 95: Coding Agent Isolation, Credentials and Network

Status: Planned. Priority: P3. Parent: Task 90. Depends on Tasks 37, 53, 89 and 91.

## Goal

Define and verify the effective trust boundary between untrusted repository code,
the coding harness, the selected runtime and Hako control-plane services.

## Decisions and work

1. Threat-model the chosen AgentCore topology and any supported MicroVM backend,
   including model tools, test subprocesses, network egress, shared filesystem,
   execution-role credential access and untrusted prompt/repository content.
2. Specify how source reads, Bedrock inference, artifacts and state updates are
   authorized for exactly one Tenant/Repository/Run/attempt. Assess whether a
   broker or other isolation boundary is required; ordinary environment-variable
   filtering is not proof that repository code cannot obtain runtime credentials.
3. Define IAM principal/resource/action restrictions, credential lifetime and
   rotation, network paths and DNS/egress enforcement, logs and redaction,
   resource budgets, and policy revocation during an active run.
4. Implement the selected boundaries default-off and test deny paths with fake
   credentials/providers. Prove effective access in an opt-in disposable AWS
   environment, including positive controls and attempted credential/egress
   access from repository-controlled code.

## Acceptance criteria

- Written trust-boundary and IAM/network specifications match implemented
  controls and distinguish application checks from enforced isolation.
- No Agent or repository process receives GitHub write credentials; Task 89
  retains sole responsibility for publication.
- AWS evidence demonstrates cross-Tenant denial and effective credential and
  egress boundaries. Without it, production Agent execution remains disabled.
