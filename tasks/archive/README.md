# 完了・置換済みタスク

ここには実装またはローカル受入が完了したタスクをまとめます。各タスク本文は依存関係や設計経緯から参照されるため元の `tasks/` 直下に残し、この一覧からリンクします。production/AWSの未実施事項は完了扱いにせず、後続タスクまたは運用ゲートで追跡します。

## 完了

- [37 — GitHub App credentials](../task-37-github-app-credentials.md) — internal library
- [38 — Workload source checkout](../task-38-workload-source-checkout.md) — checkout package, docs, acceptance tests
- [56 — Tenant GitHub App registry](../task-56-tenant-github-app-registry.md) — metadata/API/CLI, PostgreSQL acceptance
- [62 — Workload command acceptance](../task-62-workload-command-acceptance.md) — acceptance library; deployment is tracked by Task 40
- [77 — Claude Action E2E](../task-77-claude-action-e2e.md) — local acceptance; production rollout remains a separate gate
- [80 — Workspace runtime contract](../task-80-workspace-runtime-contract.md) — AWS API/SDK contract; real AWS PoC remains unverified

## ローカル実装完了（運用ゲートは別途）

以下はローカル実装・受入が終わっています。AWS配備や実環境検証は別の未完了項目として扱います。

- [41 — GitHub Checks](../task-41-github-checks.md) — Secret、network、deployment gate待ち
- [78 — Platform operator DLQ](../task-78-webhook-platform-operator-dlq.md) — operator provisioning/deployment待ち
- [79 — Webhook lifecycle E2E](../task-79-webhook-lifecycle-e2e.md) — production AWS/GitHub設定待ち
- [81 — Workspace runtime journal](../task-81-workspace-runtime-journal.md) — bucket/IAM配備待ち
- [82 — Workspace Lambda MicroVM adapter](../task-82-workspace-lambda-microvm-adapter.md) — runtime PoC/deployment待ち
- [83 — Workspace runtime recovery E2E](../task-83-workspace-runtime-recovery-e2e.md) — production AWS検証待ち
- [71–72 — Tenant App local E2E and OAuth broker mTLS](../task-71-tenant-app-local-e2e.md), [72](../task-72-oauth-broker-mtls.md) — production certificates/IAM/network待ち
- [73–75, 77 — Claude Code Action local acceptance](../task-73-claude-code-action.md) — runtime/deployment gateは未完了

## 置換済み

- [36 — Workload Worker ADR](../task-36-workload-worker-adr.md) — EC2案を置換。実行基盤の選択はTask 61で再検討

## 完了タスクの扱い

- 完了タスクは新規着手キューから除外し、この一覧から参照します。
- タスク本文は削除しません。後続タスクやコードレビューからのリンクを維持するためです。
- `Implemented locally` や `local acceptance complete` は、本番配備・外部検証まで完了した意味ではありません。残る運用作業は別の未完了タスクで追跡します。
