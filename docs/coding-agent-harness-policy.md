# Coding Agent Harness and Model Policy

Status: Initial combination selected; trusted configuration validation is implemented, while Runtime entrypoint and provider composition remain open.
Date: 2026-11-21

## Initial supported combination

| Harness | Model API | Execution target | Status |
| --- | --- | --- | --- |
| Hako Python workflow (`hako-coding-agent` 0.1.x) | Amazon Bedrock Converse through an injected client | AgentCore Runtime microVM, one Hako attempt per session | Local HTTP entrypoint and regional client composition exist; deployment and Hako dispatch/result integration remain unimplemented |

The harness owns a bounded plan, code, verify, and review loop. Its tools are
fixed in application code. Planning and review are read-only. The code phase
can read and write only files in the in-memory source workspace. Model output,
task text, and repository files are untrusted. The harness emits bounded
`agent_changes` data, which is still subject to Hako's independent verifier.

## Trusted model selection and budgets

The model ID, Bedrock Region, and inference settings come from trusted
Resource Plane deployment configuration. Do not accept them from a Run body,
repository file, workflow input, or model output. Deployment configuration
must allowlist the exact model ID and Region. A missing, disallowed, or
unavailable model fails the attempt without falling back to another model or
execution backend.

The current Python adapter bounds each call to 2,048 output tokens by default,
with a code-enforced ceiling of 4,096. It limits the workflow to a configured
number of iterations, each model phase to at most 32 turns, and tool use to at
most 128 calls per phase. The deployment should set smaller operational limits
where measured latency and cost allow. AgentCore and Hako apply a separate
attempt timeout. The current Python prototype does not yet enforce a hard
monetary spend ceiling because it does not reconcile provider usage charges.
Track token usage and usage cost before advertising a spend cap.

The current adapter sends only the fixed tool catalog, selected task context,
and bounded source text to the model. It has no shell or network tool. A future
language verifier must run outside this harness boundary with its own resource
and network controls. Prompt instructions do not enforce filesystem, process,
credential, or network isolation.

The AgentCore-specific `propose_with_policy` path validates the trusted attempt
identity and source archive digest, runs the bounded plan/code/review loop, and
returns only a draft proposal plus the supplied attempt pins. It does not run a
test command or claim verification. The existing Hako verifier must independently
measure the resulting proposed tree, and Hako must revalidate attempt freshness
and cleanup before publication eligibility. `execute` remains available for
the separate offline harness composition that injects a local verifier.

`hako_coding_agent.policy.AgentCorePolicy` is the local deployment boundary
for these settings. It requires an exact model ID from
`HAKO_CODING_AGENT_ALLOWED_MODEL_IDS` (a JSON string array), the matching
`HAKO_CODING_AGENT_MODEL_ID`, and an exact Region from
`HAKO_CODING_AGENT_ALLOWED_BEDROCK_REGIONS` matching
`HAKO_CODING_AGENT_BEDROCK_REGION`. Optional settings are
`HAKO_CODING_AGENT_MAX_ITERATIONS`, `HAKO_CODING_AGENT_MAX_TURNS`,
`HAKO_CODING_AGENT_MAX_TOOL_CALLS`, `HAKO_CODING_AGENT_MAX_TOKENS`, and
`HAKO_CODING_AGENT_ATTEMPT_TIMEOUT_SECONDS`; their defaults are 3, 12, 32,
2048, and 600 seconds, with hard ceilings of 10, 32, 128, 4096, and 900
seconds. The timeout ceiling follows the synchronous `InvokeAgentRuntime`
request limit; longer coding jobs require a separately implemented asynchronous
Runtime job contract.
Invalid, missing, duplicated, or unapproved settings fail closed. The validated
policy builds the existing Converse adapter with its model and call budgets.
`execute_with_policy` also propagates the validated iteration ceiling into the
workflow. The local Runtime entrypoint creates the Bedrock client in the
configured Region and checks the attempt deadline before and after each
Converse call. An in-flight call can exceed the local deadline by up to the
configured 60-second read timeout; the synchronous AgentCore limit is the
outer hard cap, not a provider cancellation signal. This entrypoint is
fake-tested locally, but it is not packaged or deployed and no AWS invocation
has been made.

## Deferred adapters

Do not add Claude Agent SDK or Codex harness adapters to the initial managed
Coding Agent path. Each would add separate credentials, tool semantics,
process lifecycle, model policy, and proposal conversion requirements without
an implemented AgentCore invocation and verification loop. Reconsider an
adapter only when it can satisfy the same attempt identity, bounded tool,
cancellation, result, and credential contracts with synthetic-client tests.

Claude Code Action remains supported as a GitHub Actions workflow on Hako's
one-job ephemeral Runner. It is a separate entry point and uses the Task 73–77
workflow provenance and Tenant credential policies. It does not select the
Hako Coding Agent harness.

## Implementation sequence

1. Keep the Python workflow and injected Bedrock client contract as the first
   harness baseline.
2. Pass the validated `AgentCorePolicy` through the Runtime entrypoint, create
   the regional Bedrock client, and bind the attempt timeout before invocation.
3. Bind progress and terminal results to the immutable Hako attempt and return
   only a bounded proposal plus measured verifier evidence.
4. Add provider contract tests for denied model IDs, malformed responses,
   tool limits, cancellation, provider errors, and result size.
5. Measure provider usage and latency in a disposable environment before
   selecting production defaults or claiming a hard spend limit.

See [AgentCore Runtime Placement](agentcore-runtime-placement.md) for session,
role, and Region placement decisions.
