# Task 61: Validate Lambda MicroVM Runtime Contract

Status: API contract identified (AWS Lambda MicroVMs); image/container/network PoC pending. Priority: P0. Depends on: Task 39 substrate-neutral foundations; coordinate with Task 43.

## Goal

実行基盤はユーザー指定のLambda MicroVM。具体的provider/APIとWorkload要件の適合を確認する。[ADR 0008](../docs/adr/0008-non-ec2-workload-runtime.md)。先のFargate選定は撤回済み。

## Work

- [x] Lambda MicroVM前提にADR・README・Task 40を訂正する。EC2 workerとFargate Workload workerは追加しない。
- [x] AWS Lambda MicroVMs専用APIと公式SDKを確認。RunMicrovm ClientToken、GetMicrovm、TerminateMicrovm、lambda: IAM prefixを使用する。[具体的な実装契約](../docs/lambda-microvm-workloads.md)。通常のLambda invocationとは別API。
- [x] Public provider contract: generic Hako Run deadline is 1 second–24 hours; Lambda `maximumDurationInSeconds` accepts 1–28,800 seconds. The adapter rejects sub-second/over-8-hour windows before provider calls, and the 1-second boundary maps exactly to the API. Dockerfile image building creates a MicroVM snapshot; it is distinct from running nested Docker/containerd/Podman/Compose inside the VM. See [runtime contract](../docs/lambda-microvm-workloads.md).
- [x] Record documented boundaries: Lambda MicroVMs isolate individual workloads and support VPC egress connectors whose Security Groups and NACLs govern outbound traffic; default egress is public Internet. Source/artifact transfer uses Hako's signed Agent protocol and controller-owned S3 path; the MicroVM receives no S3 credentials. Pricing dimensions are recorded without reusing us-east-1 sample rates for Tokyo. Sources: [Lambda MicroVM core concepts](https://docs.aws.amazon.com/lambda/latest/dg/microvms-how-it-works.html), [networking](https://docs.aws.amazon.com/lambda/latest/dg/microvms-networking.html), and [pricing](https://aws.amazon.com/lambda/pricing/).
- [ ] Prove Hako guest/runtime behavior on the pinned ARM64 Lambda MicroVM: nested rootless containerd/Podman and Compose, effective cgroup/namespace isolation, per-Tenant filesystem/runtime separation, source/artifact transfer, and VPC connector default-deny including AWS endpoints and public Internet. Local Podman tests do not satisfy this acceptance.
- [x] provider mappingと復旧契約を定義・library実装（Tasks 63–65）。AWSは8時間上限、ARM64のみ、標準Internet egress。24時間Runとの不一致はadapterの起動前拒否で扱う。実AWSのidempotency保持期間・raceはPoCで確認する。
- [x] Document published account/Region quotas, API rate limits, size tiers, 8-hour cap, baseline/peak billing, snapshot read/write/storage, and data-transfer billing in the runtime contract. Check live Service Quotas and prices only in the approved deployment account/Region.
- [x] Add a repeatable local artifact check that validates public-key JSON, exact ZIP contents, and a statically linked ELF64/AArch64 Agent. This does not claim image-builder or MicroVM execution support.
- [ ] Measure actual Hako image build/cold start, idle suspend/resume, run/cancel/cleanup, quota/throttling, and cost in a dedicated approved Lambda MicroVM environment. Do not treat host Podman or QEMU smoke tests as the provider PoC.

## Acceptance criteria

- provider APIと実装対象が具体的に記録され、Task 40が配備/IAMを設計できる。
- 未達要件は明示する。基盤を自動変更せず、未検証の24時間実行やnested container対応を保証しない。
- CP/RP分離、DSQLへのRP直接接続禁止、Secret非露出、attempt fencingを維持する。
- 実行pathと監視が検証されるまでRun作成はdefault-off。AWS applyは含まない。

## Local verification note (2026-10-17)

`make build-workload-microvm-artifact` produced the ARM64 agent ZIP using the
test-only public-key fixture, and the output was inspected as a statically
linked AArch64 executable plus Dockerfile and public key. A Podman image build
pulled the Lambda MicroVM base images but failed at `dnf` with `exec format
error` because the local x86 host has no ARM64 emulation. This does not satisfy
the Lambda image/container/network PoC; use Lambda's image builder or an ARM64
host for the image validation step. No AWS resources were created or changed.

## Local verification refresh (2026-10-24)

Rebuilt the ARM64 artifact from current source and confirmed the ZIP contains
only the Agent Dockerfile, statically linked AArch64 executable, and synthetic
public verification key. Re-ran the Task 40 Workload runtime/PostgreSQL
integration and Task 42 Podman Software Factory E2E successfully. The local
x86_64 host still has no ARM64 binfmt/QEMU runtime, so image build/execution,
container support inside the Lambda MicroVM, cold-start/network/isolation, and
real lifecycle/cost/quota behavior remain unverified. No AWS resources were
created or changed.

## Documentation refresh (2026-10-24)

Reviewed the current official Lambda MicroVM sizing and quota documentation.
Recorded the published baseline/peak/disk classes, account/Region aggregate
memory quotas, fixed 8-hour maximum, per-MicroVM connection ceilings, and API
TPS/burst values in [`docs/lambda-microvm-workloads.md`](../docs/lambda-microvm-workloads.md).
The Hako `standard` image proposal (4 GiB/2 vCPU baseline, 16 GiB/8 vCPU peak,
16 GiB disk) matches a published size. It remains unproven as an Hako image:
Dockerfile snapshot build is not evidence for nested Docker/containerd/Podman,
Compose, cgroup limits, Tenant isolation, cold-start, cost, or network policy.
No image build, AWS API call, or infrastructure change was performed in this
refresh.

## Image base digest pinning (2026-10-24)

Pinned the Workload Agent Go toolchain, Workload Agent Lambda MicroVM base,
and all Workspace init Lambda MicroVM stages to verified ARM64 child-manifest
digests resolved from the official Docker Hub and ECR Public versioned tags.
This makes Dockerfile base inputs immutable, but does not satisfy the
image/container/network PoC; the local x86_64 host still cannot execute ARM64
build steps. No AWS resources were created or changed.

## ARM64 user-mode smoke refresh (2026-10-25)

Pulled the public `tonistiigi/binfmt` image with Podman. Its ARM64 emulator
installed successfully inside its privileged container, but this rootless
Podman host does not expose that binfmt registration to host processes; a
normal `podman run --platform linux/arm64` still fails with `Exec format
error`. Extracted the static `qemu-aarch64` emulator to `/tmp` and used it to
execute the ARM64 `coreutils` binary from an exported copy of the official
Lambda `provided:al2023` ARM64 base image; `uname -m` returned `aarch64`. The
existing statically linked ARM64 Hako Agent binary also starts under QEMU and
fails closed on the expected missing runtime configuration. This confirms
individual ARM64 ELF execution, but not Podman/Dockerfile `RUN` emulation,
Lambda image-builder behavior, nested containers, or a real MicroVM. Temporary
emulator/rootfs files are outside the repository. No AWS resource/API was
used or changed.

## Podman Runtime Contract (2026-10-25)

`make test-job-services-podman` passes against the local Podman engine and the
production Job Service adapter. This verifies the host-side rootless Job
container contract; it does not run inside a Lambda MicroVM and therefore does
not prove nested containerd/Podman availability, MicroVM namespaces/cgroups,
Tenant isolation, or Lambda network behavior. The Lambda image and real
MicroVM PoC remain open.

## ARM64 Podman execution retry (2026-10-25)

Retried host ARM64 container execution using the privileged `tonistiigi/binfmt`
installer with the host `binfmt_misc` registration path mounted. The kernel
denied opening `/proc/sys/fs/binfmt_misc/register` (`permission denied`), and
Podman still reported only amd64/386 emulators; `podman run --platform
linux/arm64 ... uname -m` failed with `Exec format error`. This environment
cannot register the host emulator even with Podman's privileged container.
The earlier direct `qemu-aarch64` ELF smoke check remains valid only for
individual ARM64 binaries. Continue this acceptance on an ARM64 builder or a
host where binfmt registration is available, then repeat it inside the actual
Lambda MicroVM before treating Task 61 as complete. No AWS API/resource was
used or changed.

## Provider contract and cost refresh (2026-10-25)

Rechecked the AWS primary API and guide: `RunMicrovm.maximumDurationInSeconds`
has an explicit 1–28,800 second range; the adapter now has a regression test
for the 1-second request and rejects 24 hours before the provider call. The
image guide describes Dockerfile execution followed by a snapshot, while
nested container runtime support remains a separate guest capability test.
Recorded the published pricing dimensions and a Hako usage formula without
copying us-east-1 example rates into a Tokyo estimate. Local contract and
documentation acceptance is complete; image/runtime/network/quota/cost PoC
still requires an approved target MicroVM and remains open. No AWS API or
resource was used.

## ARM64 Lambda artifact inspection (2026-10-25)

Inspected the current `make build-workload-microvm-artifact` output: the ZIP
contains only the Agent Dockerfile, statically linked AArch64 `hako-agent`,
and the supplied synthetic test public key. `readelf` confirms ELF64/AArch64
for the Agent. Also inspected `make build-workload-controller-lambda`; its
`provided.al2023` bootstrap is a statically linked ELF64/AArch64 executable.
This verifies cross-compilation and package contents only. The test key cannot
authenticate production traffic, and neither artifact was executed inside a
Lambda MicroVM. Container runtime, network/isolation, and lifecycle/cost PoC
remain open. No AWS API/resource was used.

Added `make verify-workload-microvm-artifact`, which rebuilds the ZIP, checks
its exact three-file allowlist, validates the supplied Ed25519 public-key JSON,
compares the packaged key bytes with the caller input, and checks that the
Agent is static ELF64/AArch64. `go test ./cmd/hako-agent-keycheck
./internal/workloadagent -count=1` covers valid public keys, malformed/private
material, missing files, and invalid arguments. The command passed with the
synthetic test fixture. It remains a package/architecture check; it does not
build or run the container image or verify Lambda MicroVM behavior.
