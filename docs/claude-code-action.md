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
pinning the Action revision and its inputs. Local lifecycle, provenance, and
credential-isolation behavior is tested with fake GitHub/AWS/model providers;
this is not production certification. A pinned runner image, deployed
broker/worker/IAM/certificates, restricted network, and live cleanup verification
remain deployment gates. See
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
- Provenance verification is a separate gate from provider lookup and runtime
  execution. A successful check does not itself provision a runner or issue a
  model credential.

## Lambda MicroVM guest identity binding

The Actions runner guest now has a local runtime handler for the Lambda
MicroVM `/run` lifecycle hook. It strictly decodes the provider-supplied
`microvmId` and non-secret `runHookPayload`, pins Workflow Job/Run/attempt,
repository URL, runner name, and labels, then enables the one-time signed
bootstrap route. The signed bootstrap must match that exact launch identity
before the guest consumer runs. Identical `/run` retries are idempotent;
conflicting VM or job identities are rejected.

The guest executable and process consumer configure the official runner as
ephemeral, pass the Tenant model key only in the runner process environment,
and remove local credentials, diagnostics, temp, and checkout state after
exit. Local tests cover `/run` binding through signed bootstrap and fake runner
execution. A PostgreSQL lifecycle E2E now composes signed Tenant webhook
ingress, authorized admission, startup worker, Check Outbox/Dispatcher/consumer
to fake GitHub Checks, and Broker-mediated MicroVM/runner cleanup with injected
provider failures and retries. The same test exercises fork, wrong-SHA and
cross-Tenant model credential denial using the provenance verifier and
PostgreSQL resolver with a synthetic Secret reader. It does not call the live
GitHub Actions API, build/run the published Lambda MicroVM image, or verify
production IAM/network policy. A pinned ARM64 image recipe is provided, but no
runner archive/image has been built or published. AWS documents the `/run` hook body
and per-MicroVM payload in [Running and using MicroVMs](https://docs.aws.amazon.com/lambda/latest/dg/microvms-launching.html).

## Tenant setup and operating procedure

1. A platform operator registers and approves a Tenant-owned GitHub App
   credential binding and completes its Installation binding. Keep the App
   private key, OAuth client secret, and Webhook secret in purpose-specific
   Secrets Manager entries; do not put secret values in Hako API/CLI input.
2. A Tenant Owner/Admin enables Actions Runner admission for the exact
   repository and configures a data-only workflow policy:

   ```sh
   hako github repository runner enable --tenant acme --installation 123 --repository 456
   hako github repository runner policy --tenant acme --installation 123 --repository 456 --policy-file policy.json
   ```

   Use an allowlisted branch/event and only required Hako labels. Fork jobs,
   `pull_request_target`, unapproved refs, foreign head repositories, and
   unknown labels are denied. Disable the repository runner admission when it
   is no longer needed; disabling also clears its workflow policy.
3. Configure the Tenant-owned model credential separately using the
   Owner/Admin CLI and an operator-approved binding. See
   [`claude-model-credentials.md`](claude-model-credentials.md) for immutable
   VersionId, rotation, disable, and Secret/Role boundaries. The model key is
   never a GitHub Actions secret or Hako App credential.
4. Pin every third-party Action to a reviewed full commit SHA. Do not allow
   untrusted PR content to reach a model credential or privileged network.
   Test policy changes locally with
   `make test-actions-runner-lifecycle-integration`; this uses isolated local
   PostgreSQL schemas and synthetic credentials.

### Audit and incident response

Repository admission and workflow policy changes are recorded as
`tenant.github_repository.actions_runner.configure` and
`tenant.github_repository.actions_runner.workflow_policy.configure` audit
events. Model credential create/rotate/disable operations use
`tenant.model_credential.create`, `.rotate`, and `.disable`. Correlate those
records with the integration, Installation, repository, workflow job/run
attempt, commit SHA, Resource Plane and MicroVM identifiers in the durable
runner/Check/cleanup records. Audit metadata must not contain credential bytes,
Secret values, runner registration tokens, or model API keys.

For a suspected prompt/workflow/key exposure: disable the Tenant model
credential with its current generation; disable the repository runner opt-in
and workflow policy; revoke the operator-managed Secret reader Role/Secret
binding if access may be compromised; then review the Tenant audit and runner
cleanup records. Confirm the MicroVM is terminated and the GitHub ephemeral
runner is deregistered before closing the incident. Rotate by creating a new
immutable Secret version and using the observed generation; do not overwrite a
pinned version. If cleanup is pending, retain the durable row and retry via the
cleanup worker rather than manually declaring the resource absent.

### Production rollout gates

Local tests and mock Terraform plans do not enable production admission. Before
deployment, review and separately approve the exact ARM64 runner archive/hash
and image digest, Lambda MicroVM `/run` contract, VPC connector and egress
policy, queue/DLQ and visibility/timeout settings, least-privilege IAM and
purpose-specific Secret reader Roles, mTLS certificate issue/rotation, broker
and worker deployment, Check delivery, alarms, and cleanup/recovery drills.
Keep default-off features disabled until a staged live smoke test proves
success, failure, cancellation, timeout and physical cleanup. No AWS apply,
real Secret read, or GitHub installation change is authorized by this runbook.
