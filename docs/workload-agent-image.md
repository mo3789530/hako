# Workload Agent MicroVM Image

`cmd/hako-agent` is the bounded Workload execution agent. The image is a Lambda
MicroVM code artifact (a ZIP containing a Dockerfile and its build context),
not an OCI image to deploy as a Lambda function. The local packager cross-builds
the ARM64 agent and includes only a caller-supplied public verification-key
JSON file:

```sh
make build-workload-microvm-artifact \
  HAKO_AGENT_TRUSTED_KEYS_FILE=/secure/build-inputs/agent-public-keys.json
```

The output is `build/hako-workload-microvm.zip`. The build requires a non-empty
file, but does not retrieve credentials or inspect AWS. The file format is a
JSON map from key ID to unpadded base64url Ed25519 public key. The checked-in
`images/workload-agent/trusted-keys.example.json` is test-only and cannot
authenticate a real controller; do not use it for a deployable image. Private
signing keys remain in the Resource Plane Controller and never enter this
artifact.

## Hook configuration

Configure the MicroVM image `hooks` object with `port: 8080`, image build hooks
`ready: ENABLED` and `validate: ENABLED`, and the MicroVM lifecycle hook
`run: ENABLED`. For example:

```json
{
  "port": 8080,
  "microvmImageHooks": {
    "ready": "ENABLED",
    "readyTimeoutInSeconds": 300,
    "validate": "ENABLED",
    "validateTimeoutInSeconds": 300
  },
  "microvmHooks": {
    "run": "ENABLED",
    "runTimeoutInSeconds": 60
  }
}
```

`/ready` and `/validate` return HTTP 200 after the listener is available. On a
new allocation, `/run` receives the provider's `microvmId` plus the
Controller-created `runHookPayload`. The hook validates the bounded JSON and
pins the exact MicroVM ID, Workload attempt identity, source digest, commit,
and immutable deadline. An identical retry is idempotent; another VM or
attempt cannot replace the pin. The production `cmd/hako-agent` requires this
pin before accepting protocol requests and rejects tickets whose VM, attempt,
or signed execution binding differs. No repository code or secret is executed
by the hook.

The Dockerfile uses the Lambda AL2023 MicroVM container base, copies in the
static ARM64 agent and Go toolchain, installs Git, and starts the listener on
the only exposed application port as UID/GID 65532, not root. Per-VM identity
is established after the snapshot by `/run`; do not bake Tenant identity, run
payloads, credentials, or secret values into the image. For signing-key
rotation, build an image that contains both old and new public keys, switch
the Controller signer, then remove the old key in a later image after old
allocations are drained. Keep the image version and digest pinned in the
Resource Plane runtime profile so rollback selects a reviewed prior version.

## Verification boundary

Local tests cover ready/run hooks, strict and bounded decoding, duplicate and
conflicting initialization, and rejection of tickets bound to another VM,
attempt, or execution specification. `make build-workload-microvm-artifact`
proves the ZIP packaging and ARM64 binary build only; CI exercises it with the
test-only public key fixture. The Dockerfile has not
been built by Lambda's image builder, and no AWS image, hook, IAM, network, or
MicroVM lifecycle has been deployed or verified. The test-only key example is
used for packaging checks only.

On the local development host, the ARM64 ZIP package builds successfully and
contains the expected three files. A Podman ARM64 image build currently stops
at the first `RUN` instruction with `exec format error` because this host has no
ARM64 execution emulation configured. Do not treat an amd64 build, the ZIP
package check, or the pulled base image as proof of an ARM64 Lambda MicroVM
image build; use Lambda's image builder or an ARM64 build host for that gate.

This image currently supports the Workload protocol's fixed Go test operation;
it does not implement the interactive Workspace shell/PTY API. The guest runs
tests with network access disabled by its current process factory, so projects
requiring uncached Go modules remain unsupported until a separately reviewed
dependency-cache or egress policy is implemented.
