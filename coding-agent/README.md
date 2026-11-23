# Hako Coding Agent

Python 3.14 implementation of the coding workflow intended for AgentCore Runtime.
The initial workflow core composes a bounded in-memory workspace, injected
plan/code/review and verifier adapters, and proposal generation. A Bedrock
Converse model adapter accepts a configured client and model ID, and exposes
bounded in-memory file tools. A production client composition, source and
verifier adapters are not implemented, and it cannot yet run a real task or be
deployed. No live Bedrock request has been tested.
It must not be used as publication evidence. Hako's independent verifier and
publication policy remain authoritative.

`hako_coding_agent.execution.execute` composes a source digest check, model,
workspace and verifier for one attempt. Its local result contains the supplied
attempt pins and returns proposal bytes only after a locally verified run.
The caller still must authenticate those pins; this function does not provide
dispatch fencing, cleanup, cancellation or independent verification.

Run the offline tests from the repository root:

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=coding-agent/src python3 -m unittest discover -s coding-agent/tests -v
```

Install the Python development tools with `python3 -m pip install -e "coding-agent[dev]"`.
Run `make lint-python` from the repository root to check formatting, lint rules
and strict types. Run `make test-python` to execute the coding agent tests with
pytest.
Run `make test-python-coverage` to report line and branch coverage, including
missing lines. CI requires at least 90% combined coverage.

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

The next implementation steps are attempt-authorized source and task input, configured
Bedrock client with network and IAM restrictions, broader source support, a verifier
adapter, AgentCore entrypoint, durable
result handoff and integration with the attempt lifecycle in Task 90. A tree
digest in the workflow is supplied by an adapter; until a trusted digest
implementation exists it is not proof of the tested proposal.
