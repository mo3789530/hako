# Hako Coding Agent

Python implementation of the coding workflow intended for AgentCore Runtime.
The local workflow composes a digest-checked in-memory source snapshot, bounded
plan/code/review tools, deployment-allowlisted Bedrock Converse model policy,
and an HTTP Runtime entrypoint (`/ping` and `/invocations`). The deployment
entrypoint creates a regional `boto3` client with bounded transport timeouts.
No live Bedrock or AgentCore Runtime request has been tested. The ARM64 image
definition is in `coding-agent/Dockerfile`, but it has not yet been built in
this environment. The HTTP handler validates payload shape;
the trusted Resource Plane caller must authenticate attempt pins. It must not
be used as publication evidence. Hako's independent verifier, cleanup fence,
and publication policy remain authoritative.

`hako_coding_agent.execution.execute` composes a source digest check, model,
workspace and verifier for one attempt. Its local result contains the supplied
attempt pins and returns proposal bytes only after a locally verified run.
The caller still must authenticate those pins; this function does not provide
dispatch fencing, cleanup, cancellation or independent verification.

`propose_with_policy` is the AgentCore draft path: it requires an
`AgentCorePolicy` built from trusted model and Region allowlists, validates the
attempt identity and source digest, and runs only the bounded plan/code/review
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

The proposal encoder creates bounded deterministic ZIPs from captured file bytes;
an integration test passes its output to Hako's Go inspector. It does not capture
filesystem changes or support deletions and mode changes.

The model receives advisory language hints for Python, Java, Go, Rust,
TypeScript and Terraform (including `.tfvars`), based on visible file names and
known manifests. Mixed-language repositories are supported by the file tools.
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
