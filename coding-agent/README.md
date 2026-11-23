# Hako Coding Agent

Python 3.14 implementation of the coding workflow intended for AgentCore Runtime.
The LangGraph workflow composes a bounded in-memory workspace, injected
plan/code/test/review and verifier adapters, and proposal generation. The
Bedrock Converse adapter and an HTTP Runtime entrypoint support locally tested
composition. The HTTP handler validates payload shape and relies on a trusted
Resource Plane caller to authenticate attempt pins. No live Bedrock or AgentCore
Runtime request has been tested. Hako's independent verifier, cleanup fence and
publication policy remain authoritative.

`hako_coding_agent.execution.execute` composes a source digest check, model,
workspace and verifier for one attempt. Its local result contains the supplied
attempt pins and returns proposal bytes only after a locally verified run.
The caller still must authenticate those pins; this function does not provide
dispatch fencing, cleanup, cancellation or independent verification.

Install the Python development tools with `python3 -m pip install -e "coding-agent[dev]"`.
`propose_with_policy` is the AgentCore draft path: it requires an
`AgentCorePolicy` built from trusted model and Region allowlists, validates the
attempt identity and source digest, and runs only the bounded plan/code/test preparation/review
loop. It returns a digest-bound proposal draft with attempt metadata but makes
no verification claim and executes no repository tests. Hako must independently
verify the proposal, revalidate the current attempt, and confirm runtime cleanup
before using it as publication evidence. The caller still owns authenticated
dispatch, durable cancellation, and result delivery. The local deadline is
checked around each model call; an in-flight Bedrock request can continue until
its configured 60-second read timeout.

Run the offline tests from the repository root:

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=coding-agent/src python3 -m unittest discover -s coding-agent/tests -v
```

Run `make lint-python` from the repository root to check formatting, lint rules
and strict types. Run `make test-python` to execute the coding agent tests with
pytest.
Run `make test-python-coverage` to report line and branch coverage, including
missing lines. CI requires at least 90% combined coverage.

The graph runs plan, code, test preparation, verification and review nodes in order. The
test preparation node writes or updates tests; the verifier runs checks on the resulting tree. It retries
within the configured iteration limit and only marks matching verified and
approved trees as verified. It uses no checkpointer or persistent graph state;
attempt authorization and independent verification remain caller obligations.

`BedrockCodingModel(..., prompt_cache=True)` opts into a Bedrock Converse cache
checkpoint on the initial user prompt for each model call. Use it only with a
model and Region that support message checkpoints. The prefix includes task text
and language guidance; configure tenant data handling accordingly. Bedrock may
not cache a prefix below the model's minimum token count, and cache hits are not
guaranteed. Cache writes can affect cost. The default is off; no live Bedrock
cache behavior has been verified.

For an AgentCore Runtime composition, install `hako-coding-agent[agentcore]`,
set `HAKO_AGENT_BACKEND=agentcore`, `HAKO_AGENT_TENANT_ID` and
`HAKO_AGENT_REPOSITORY_ID` to the trusted deployment's fixed repository scope,
and pass `RuntimeConfig.from_env(...)` and an
`AttemptRunner` to `hako_coding_agent.agentcore.create_app`. The runner requires
an injected resolver that authenticates and authorizes invocation input before
loading source, plus a model and verifier. `HAKO_BEDROCK_PROMPT_CACHE=true`
enables prompt caching when constructing the model through `config.bedrock_model`.
`HAKO_AGENT_OBSERVABILITY=true` enables outcome and duration logs in the runner
and stage, iteration, status and duration logs for each workflow step;
it does not configure AWS tracing or log delivery. Settings default to local,
cache off and logging off. Each AgentCore deployment is restricted to one tenant
and repository; resolved attempts outside that scope are rejected before execution.
Payloads cannot choose these settings. The resolver must still authenticate the
caller and authorize the exact current attempt; repository scope alone is not
authentication. Storage, runtime IAM and network isolation remain deployment gates.

`checkpoint.encode_workspace` and `restore_workspace` serialize changed files
against a digest-pinned source and attempt. `SQLiteCheckpointStore` provides a
local revision-checked store with explicit deletion. Its caller owns storage
permissions, encryption, retention and the integrity of stored bytes; a digest
stored alongside untrusted bytes does not authenticate them. It is not a lease,
distributed fence or automatic workflow resume mechanism.
SQLite keys derive from the full attempt identity, including tenant and repository,
and stored payload identity is checked on save and load. These checks do not
authenticate the caller or enforce tenant access to the database; only a trusted
component may provide an authorized attempt and access the storage connection.
AgentCore responses include the attempt pins for downstream validation.

This is an adapter for a trusted deployment composition, not a complete
deployment. Fenced invocation, cancellation, cleanup, authorized source and
result delivery remain unimplemented. AgentCore Memory checkpoint/resume and
the managed Harness are not wired in. The in-memory workspace and external
side effects cannot safely be resumed from graph state alone.
See [repository isolation](../docs/agent-repository-isolation.md) for the proposed
Runtime, identity and access boundaries and outstanding deployment gates.

The proposal encoder creates bounded deterministic ZIPs from captured file bytes;
an integration test passes its output to Hako's Go inspector. It does not capture
filesystem changes or support deletions and mode changes.

The model receives advisory language hints for Python, Java, Go, Rust,
TypeScript and Terraform (including `.tfvars`), based on visible file names and
known manifests. Planning guidance covers each detected language and asks the model to inspect
the actual project configuration and tests. Mixed-language repositories are supported by the file tools.
Detection never selects or runs a build, test, Terraform plan or apply command.
The local tests use fake model and verifier adapters; real toolchains and
language-specific verification have not been integrated. Existing protected
path restrictions still apply, including `infra/` and `terraform/` directories.

The source loader validates the SHA-256 of an uncompressed Hako source tar and
loads it without writing to the host filesystem. Protected paths, executable
files and binary files are retained in the snapshot; protected paths are hidden
from the model, binary files cannot be read with text tools, and executable
files cannot be changed. Symlinks, special files and oversized files are
rejected. The workspace does not sandbox Python code. Its digest binds file
content, paths and executable bits in the loaded mapping, not Hako's independent
verifier source.

The Resource Plane now resolves task text and an exact digest-checked source
archive from trusted attempt-bound providers, builds the request consumed by
this Runtime, and validates the attempt-bound draft response. The lifecycle
worker, cleanup-before-publish gate, and fakeable AgentCore SDK adapter are
implemented in Go. Remaining work includes wiring the worker to SQS/Lambda,
renewing the journal lease during execution, connecting broker-backed source
and proposal storage in the deployment, completing cancellation behavior, and
providing deployed IAM/network/isolation evidence. The Runtime image pins the
official Python 3.13 slim Bookworm multi-architecture manifest digest and all
Python runtime wheels by SHA-256, runs as UID/GID 10001, and exposes the
implemented port 8080 HTTP entrypoint. Re-export locked dependencies with
`uv export --format requirements-txt --extra runtime --no-dev --no-emit-project
--no-annotate --no-header --output-file requirements-runtime.txt` from this
directory. Build locally with `make build-agentcore-coding-agent-image`; the
target uses Kiri and builds `linux/arm64` without publishing. AgentCore requires
ARM64 and its default HTTP contract uses `/ping` and `/invocations` on port
8080. A tree digest in the local workflow is still only a draft property and
is not proof of independent verification.
