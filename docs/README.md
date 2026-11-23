# Hako Documentation

Hakoの文書を、読む目的ごとにまとめた入口です。実装の現在地はリポジトリのコードと各文書の状態記述を確認してください。将来計画は[Software Factoryロードマップ](software-factory.md)、実装タスクと進捗は[タスク一覧](../tasks/README.md)で管理しています。

## まず読む

1. [Hakoアーキテクチャ概要](../README.md) — システム全体と主要コンポーネント
2. [開発環境](development.md) — ローカル開発、テスト、Terraform検証
3. 関心のある領域の設計・運用文書 — 以下の分類から選択
4. [ADR一覧](#設計判断adr) — 採用した設計判断とその理由

## システム設計と開発

- [Control Plane API契約](api-contract.md)
- [API GatewayとGo Lambda](api-gateway-lambda.md)
- [Control Plane Terraform](control-plane-terraform.md)
- [Terraform modules](terraform-modules.md)
- [Security review](security-review.md)
- [AWS resource ownership tags](aws-resource-tags.md)
- [AWS integration E2E](aws-e2e.md)
- [Fake Resource ControllerとRuntime](fake-resource-controller.md)

## 認証、Tenant、権限、配置

- [Cognito API認証](cognito-api-auth.md)
- [Cognito CLIログイン](cognito-cli-login.md)
- [Tenant identityとRBAC](tenant-identity-and-rbac.md)
- [Tenant quotas](tenant-quotas.md)
- [Tenant placement policy](tenant-placement-policy.md)
- [SchedulerとWorkspace placement](scheduler-placement.md)
- [Resource Plane registration](resource-plane-registration.md)
- [Resource Plane capacity](resource-plane-capacity.md)
- [Resource Plane health](resource-plane-health.md)

## Workspaceと開発環境

- [Workspace作成フロー](workspace-create-flow.md)
- [Workspace lifecycle API](workspace-lifecycle-api.md)
- [Workspace上限と閲覧権限](workspace-limits-and-visibility.md)
- [Workspace Lambda MicroVM契約](workspace-lambda-microvm.md)
- [Workspace Runtime Adapter](workspace-runtime-adapter.md)
- [Workspace Runtime Operations](workspace-runtime-operations.md)
- [Workspace Runtime Journal](workspace-runtime-journal.md)
- [Workspace EFS storage](workspace-efs-storage.md)
- [Workspace Gateway](workspace-gateway.md)
- [Workspace CLI connections](workspace-cli.md)
- [Workspace Agent shellとPTY](workspace-agent-shell.md)
- [Workspace connection lifecycle](workspace-connection-lifecycle.md)
- [Workspace Session Ticket](workspace-session-ticket.md)
- [Workspace Service secrets](workspace-service-secrets.md)

## Resource Planeと非同期処理

- [Resource Controller Lambda](resource-controller-lambda.md)
- [Resource Plane command/result protocol](resource-plane-command-protocol.md)
- [Resource Plane queuesとDLQ](resource-plane-queues-and-dlq.md)
- [Outbox Dispatcher](outbox-dispatcher.md)
- [Workspace Reconciler](reconciler.md)
- [Operation状態遷移とイベント履歴](operation-state-transitions.md)
- [Operation Result Consumer](operation-results.md)
- [Lambda MicroVM Workload Controller](lambda-microvm-workloads.md)
- [運用Runbook](operations-runbook.md)

## GitHub、Software Factory、Coding Agent

- [Software Factoryロードマップ](software-factory.md)
- [Software Factory workloads](software-factory-workloads.md)
- [Software Factory AWS smoke runbook](software-factory-aws-smoke.md)
- [Tenant別GitHub App設計](tenant-github-apps.md)
- [GitHub App Installation setup](github-installation-setup.md)
- [GitHub App credentials](github-app-credentials.md)
- [GitHub Credential Broker](github-credential-broker.md)
- [GitHub Webhook Processor](github-webhook-processor.md)
- [GitHub Checks API integration](github-checks.md)
- [Actions Runner startup queue](actions-runner-startup-queue.md)
- [Hako Coding Agent Platform](coding-agent-platform.md)
- [Workload Agent MicroVM image](workload-agent-image.md)
- [Workload Controller deployment](workload-controller-deployment.md)
- [Claude Code Action contract](claude-code-action.md)
- [Claude model credentials](claude-model-credentials.md)
- [Tenant Software Catalog](software-catalog.md)
- [Pull Request Preview lifecycle](preview-lifecycle.md)
- [Preview Runtime command contract](preview-runtime-command-contract.md)

## CLIとE2E

- [Authenticated CLI lifecycle E2E](cli-lifecycle-e2e.md)

## 設計判断（ADR）

- [ADR 0001: リポジトリ構成](adr/0001-repository-layout.md)
- [ADR 0002: Terraformのstateとmodule境界](adr/0002-terraform-layout.md)
- [ADR 0003: APIと非同期Operationの互換性](adr/0003-api-compatibility.md)
- [ADR 0004: 共通設定、ログ、エラー処理](adr/0004-service-basics.md)
- [ADR 0005: OCC retry、transaction境界、冪等性](adr/0005-transaction-retry-and-idempotency.md)
- [ADR 0006: Workload Worker実行基盤](adr/0006-workload-worker-substrate.md)
- [ADR 0007: Tenant専用GitHub App](adr/0007-tenant-github-apps.md)
- [ADR 0008: Lambda MicroVM Workload Runtime](adr/0008-non-ec2-workload-runtime.md)
- [ADR 0009: Lambda MicroVM上のWorkspace lifecycle](adr/0009-workspace-lambda-microvm-lifecycle.md)
- [ADR 0010: Customer-Owned Resource Planes](adr/0010-customer-owned-resource-planes.md)
- [ADR 0011: Multi-Region Resource Plane placement](adr/0011-multi-region-resource-planes.md)
- [ADR 0012: Persistent development databases](adr/0012-persistent-development-databases.md)
- [ADR 0013: Usage metering and billing](adr/0013-usage-metering-and-billing.md)

## 作業タスク

実装の順序、依存関係、進捗、未完了項目は[タスク一覧](../tasks/README.md)を参照してください。タスク別の詳細仕様は同じディレクトリの `task-*.md` にあります。
