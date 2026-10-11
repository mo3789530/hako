# Security review — 2026-10-11

対象は現在のAPI/CLI、Tenant RBAC、GitHub OAuth/Webhook、credential libraries、固定commit checkout、local workload隔離と配備template。コード確認・ローカルテスト・Go公式の既知脆弱性検査を実施する。実AWS IAM、Gateway/EFS/MicroVMの隔離、container image全体と実環境の侵入試験は未検証であり、本番安全性の完了判定ではない。

## 検出事項と修正

| 項目 | 検出と対応 | 状態 |
|---|---|---|
| Go依存関係 | 初回govulncheckで呼出経路のある11件を検出。Go 1.27.1の9件、pgx 5.8.0のGO-2026-5004、x/text 0.40.0のGO-2026-6629。Go 1.27.2、pgx 5.9.2、x/text 0.41.0へ更新 | 再検査で `No vulnerabilities found.`。到達経路の検出は全件がHakoで実際に悪用可能という証明ではない |
| OAuth credential redirect | OAuth endpointから307/308を返されると標準HTTP clientがclient secret/codeのPOST bodyを転送可能。exchangeとuser-token requestでredirect拒否、caller clientをcopyしtimeoutを制限 | 回帰テストを追加 |
| Tenant OAuth identity | integration/App/revision/generationをstateに固定し、callbackのApp一致と設定失効を再検証。shared verifierへのfallbackとowned Installationの再assignmentを拒否 | adapter/local検証。production OAuth broker未配備 |
| Webhook identity/retry | optional App ID照合、generation/Installation再検証。DB障害を失効として隔離せず再試行 | 前回実装・検証済み |
| Audit改ざん | API DB roleの広いDML grantはaudit UPDATE/DELETEも許可。bootstrap templateでaudit_events/platform_audit_eventsのUPDATE/DELETEをrevoke | template修正。実環境へのgrant適用は未実施 |

## 確認した境界

- Cognito verifierはRS256、issuer、client_id、token_use、期限を検証し、API scopeとDB Tenant membershipを別に確認する。
- MemberのWorkspaceはowner_idで絞り、Owner/Adminの表示・操作もTenant内に限定する。越境はhidden 404。Tenant App設定はOwner/Adminのみ。
- Webhookはbounded raw bodyのHMACを検証してからnormalize・Tenant処理へ進む。owned routeでは承認metadataから一つのSecretだけを解決し、shared fallbackしない。
- private-key/Webhook readerは承認済みexact Role/Secretと固定VersionIdを使う。private keyのAPI直接読取は未配備。Run identityとcache generationを都度再検証する。
- checkoutは認可されたGitHub Repositoryと固定SHAを使い、tokenはaskpass FDで渡す。hook/submodule/redirect、環境継承を抑止する。
- local Fake Jobはnetworkなし・read-only・非root・capabilities全削除のPodman内で実行し、workerのAWS/GitHub資格情報を渡さない。これは本番MicroVM隔離の実証ではない。

## 残るリスク・受入条件

- **公開Webhookの負荷制御**: Tenant ingressではheader preflightの後、active integrationに限定したDSQL共有rate counterとAPI Gateway route throttleをHMAC/Secret取得より前に適用する（Task 68）。default-offのため、これらの制限はAWSへ配備されるまで有効にならない。429/DB failureの監視と実環境負荷の確認が残る。
- **trusted brokerの侵害範囲**: processのAssumeRole許可が複数Tenant readerに及ぶ場合、そのbroker侵害の影響も複数Tenantへ及び得る。IAM trust/KMS/Secret resource policyを実環境で確認し、用途とTenantを限定する。
- **実Runtime/通信経路**: MicroVM guest network/container権限、EFS Access Point越境、Gateway session ticketのreplay/revocation、agent相互認証は後続実装とAWS PoCの受入条件。
- **運用**: 監査の保持/export、callback query非記録、readアクセス監査、Secret rotation、DLQ redrive、backup/recovery演習、container image scanは未完了。Secret値を取得して確認しない。

## 再検証

```sh
go version
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
go vet ./...
go test ./...
HAKO_TEST_DATABASE_URL='postgres://…' go test -tags=integration ./... -count=1
```

CIにgovulncheckを追加。検出がある場合はCIを失敗させる。依存関係・toolchain更新だけで既存のAWS binaryが置き換わることはないため、承認されたrelease時に修正版artifactを再buildして配備する。
