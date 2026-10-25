# Task 61: Validate Lambda MicroVM Runtime Contract

Status: API contract identified (AWS Lambda MicroVMs); image/container/network PoC pending. Priority: P0. Depends on: Task 39 substrate-neutral foundations; coordinate with Task 43.

## Goal

実行基盤はユーザー指定のLambda MicroVM。具体的provider/APIとWorkload要件の適合を確認する。[ADR 0008](../docs/adr/0008-non-ec2-workload-runtime.md)。先のFargate選定は撤回済み。

## Work

- [x] Lambda MicroVM前提にADR・README・Task 40を訂正する。EC2 workerとFargate Workload workerは追加しない。
- [x] AWS Lambda MicroVMs専用APIと公式SDKを確認。RunMicrovm ClientToken、GetMicrovm、TerminateMicrovm、lambda: IAM prefixを使用する。[具体的な実装契約](../docs/lambda-microvm-workloads.md)。通常のLambda invocationとは別API。
- [ ] 1秒〜24時間のRun deadline、Docker/containerd/Podman、compose、Tenant隔離、untrusted code、network制限、source/artifact搬送を確認する。
- [x] provider mappingと復旧契約を定義・library実装（Tasks 63–65）。AWSは8時間上限、ARM64のみ、標準Internet egress。24時間Runとの不一致はadapterの起動前拒否で扱う。実AWSのidempotency保持期間・raceはPoCで確認する。
- [ ] cold start、quota、費用、container実行、停止/cleanupのPoCを記録する。Workspace向け能力はTask 43と共有する。

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
