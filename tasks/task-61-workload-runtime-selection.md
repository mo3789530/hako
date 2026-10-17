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
