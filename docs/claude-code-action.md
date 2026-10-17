# Claude Code Action workflow contract

Hako treats Claude Code Action as a user-authored GitHub Actions workflow that
runs on an ephemeral Hako Actions runner. Hako does not synthesize workflows,
interpret Issue/PR comments as shell commands, or change GitHub Actions YAML
semantics.

## Runner labels and policy

Request the Hako labels in the job itself, for example:

```yaml
jobs:
  claude:
    runs-on: [self-hosted, hako, arm64]
    steps:
      - uses: actions/checkout@<reviewed-full-commit-sha>
      - uses: anthropics/claude-code-action@<reviewed-full-commit-sha>
        with:
          prompt: "Review the changes in this pull request."
```

Use a full, reviewed commit SHA for every third-party Action reference. Hako
does not bundle an Action version, move tags, or claim compatibility with an
unverified release. The repository owner is responsible for reviewing and
pinning the Action revision and its inputs. Actual Claude Action execution is
not certified until Task 75 runtime delivery/IAM wiring and Tasks 76–77
(execution and E2E) are complete. See
[`claude-model-credentials.md`](claude-model-credentials.md) for the
Tenant-owned provider credential model.

Configure the repository policy with the workflow file path, approved branch
refs/events, and allowed runner labels. For the example above, the policy's
`workflow_path` must match the file that contains the job; `required_labels`
can be `["self-hosted", "hako"]`, while `allowed_labels` can additionally
include `arm64`. The allowlist contains no commands, credentials, image names,
IAM roles, or network selectors. Only Tenant Owners/Admins can change it.

## Current admission limits

- The broker checks the exact workflow run, attempt, job, repository, commit,
  branch, event, workflow path, job name, and labels against GitHub's read-only
  Actions API and the admitted webhook identity before consuming the claim.
- Fork-originated jobs and `pull_request_target` fail closed. Workflow refs must
  be an explicitly allowed `refs/heads/...` ref. PR merge refs are not accepted
  by this policy contract.
- Only repository-scoped `actions:read` is used for provenance lookup. It is
  distinct from checkout, runner registration, and any future model credential.
- This gate does not itself provision the runner, execute the Action, or issue a
  Claude model credential. Those paths remain separate tasks and are not
  implied by a successful provenance check.
