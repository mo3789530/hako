# Security review — 2026-10-25

対象は現在のAPI/CLI、Tenant RBAC、GitHub OAuth/Webhook、credential libraries、固定commit checkout、local workload隔離と配備template。コード確認・ローカルテスト・Go公式の既知脆弱性検査を実施する。実AWS IAM、Gateway/EFS/MicroVMの隔離、container image全体と実環境の侵入試験は未検証であり、本番安全性の完了判定ではない。

## 検出事項と修正

| 項目 | 検出と対応 | 状態 |
|---|---|---|
| Go依存関係 | 初回検査で呼出経路のある依存脆弱性を修正し、pgx 5.9.2/x/text 0.41.0へ更新済み。2026-10-25の再検査でGO-2026-6809 (`crypto/x509`、CVE-2026-111943)を検出し、Go 1.27.3へtoolchain/build imageを更新。更新後の`govulncheck@v1.8.0 ./...`は`No vulnerabilities found`を返した | Go公式は1.27.3をx509 security fixと記録。govulncheckは依存モジュールに未到達5件を報告するが、呼出経路は見つけていない |
| OAuth credential redirect | OAuth endpointから307/308を返されると標準HTTP clientがclient secret/codeのPOST bodyを転送可能。exchangeとuser-token requestでredirect拒否、caller clientをcopyしtimeoutを制限 | 回帰テストを追加 |
| Tenant OAuth identity | integration/App/revision/generationをstateに固定し、callbackのApp一致と設定失効を再検証。shared verifierへのfallbackとowned Installationの再assignmentを拒否 | adapter/local検証。production OAuth broker未配備 |
| Webhook identity/retry | optional App ID照合、generation/Installation再検証。DB障害を失効として隔離せず再試行 | 前回実装・検証済み |
| Audit改ざん | API DB roleの広いDML grantはaudit UPDATE/DELETEも許可。bootstrap templateでaudit_events/platform_audit_eventsのUPDATE/DELETEをrevoke | template修正。実環境へのgrant適用は未実施 |
| Artifact cleanup error propagation | Cleanup Workerは構造化ログをredactしていたが、返却errorにS3/DSQL詳細やArtifact IDを含める経路があった。Workerの戻り値を固定sentinelへ変更し、失敗分類と相関IDだけをlogへ残す | list/delete/release/oversized batchのunit testを追加。Lambdaへ渡るerror文面もsanitize |
| Runtime/Gateway startup failure logs | Resource Controller LambdaのAWS SDK、runtime、queue、handler初期化失敗とWorkspace Gatewayの終了ログが元error文字列を出す経路を除去。固定`failure_class`だけを記録 | local source review。provider詳細、endpoint、filesystem pathをログへ渡さない |
| Workspace port forwarding | GatewayはSession Ticketのactive lookup後、port-forward scopeを要求。Previewはgeneration-specific identityのTCP/3000のみ。通常WorkspaceはResource Plane HMAC署名APIを経由し、persisted Session、Tenant/Workspace/Plane、現在Placement、running状態、healthyかつ現行MicroVM世代のService、Catalog宣言TCP portを照合。制御APIへ渡るのはclaimsとportのみ | PostgreSQL統合テストが宣言外port、retired generation、unhealthy Service、suspended Workspace、Placement変更、identity mismatchを拒否。実Gateway/AWS接続は未検証 |
| Process/Lambda failure boundary | API、Dispatcher、Webhook processor、result consumer、Reconciler、Runner、Workspace Service、GitHub broker、scheduled expiry/idle handlerの起動失敗を固定分類化。scheduled handlerは再試行用の固定errorだけを返す | 対象command test通過。既知の`log.*(err)`/`%v`形式をsource scanで除去 |

## 確認した境界

- APIは全リクエストにサーバー生成の`X-Hako-Request-ID`を返し、route template、status、durationと共に構造化ログへ記録する。入力された同名Headerは採用しない。GitHub webhook ingressも同じIDを信頼済みrequest context経由でInboxに保存し、repository-triggered Workload RunとActions Runner Checkに引き継ぐ。Actions Runner Checkを含むCheck Outboxは相関IDを保持し、consumerは永続Checkと一致するIDだけ配信してstructured delivery logに記録する。Workload Runの相関IDはDB・transactional Scheduler Outbox/SQS・認証済みResource Plane execution metadata・Lambda MicroVM run hook・署名付きAgent execution binding・terminal result・GitHub Check Outboxまで伝播し、resultは永続Runと一致する場合だけ適用する。artifact metadataは所有RunからIDを解決する。個別worker/API failure logへの相関ID追加は未完了。

- Cognito verifierはRS256、issuer、client_id、token_use、期限を検証し、API scopeとDB Tenant membershipを別に確認する。
- MemberのWorkspaceはowner_idで絞り、Owner/Adminの表示・操作もTenant内に限定する。越境はhidden 404。Tenant App設定はOwner/Adminのみ。
- Webhookはbounded raw bodyのHMACを検証してからnormalize・Tenant処理へ進む。owned routeでは承認metadataから一つのSecretだけを解決し、shared fallbackしない。
- private-key/Webhook readerは承認済みexact Role/Secretと固定VersionIdを使う。private keyのAPI直接読取は未配備。Run identityとcache generationを都度再検証する。
- checkoutは認可されたGitHub Repositoryと固定SHAを使い、tokenはaskpass FDで渡す。hook/submodule/redirect、環境継承を抑止する。
- local Fake Jobはnetworkなし・read-only・非root・capabilities全削除のPodman内で実行し、workerのAWS/GitHub資格情報を渡さない。これは本番MicroVM隔離の実証ではない。
- Workspace Gatewayのrequest failureは固定failure classとSessionから検証済みのTenant/Workspace/Plane/permissionだけを記録し、Ticket、provider error、storage referenceは記録しない。Artifact cleanup Workerは固定failure classと信頼済みRun/artifact/correlation identityを記録し、provider/DB errorは戻り値にも含めない。Previewにはgeneration/attempt-fenced cleanup leaseとretry orchestratorがあるが、Lambda MicroVM/Gateway routeを削除するprovider-backed executorは未接続である。metadataの`cleaned`状態をprovider削除の証明として扱わない。
- Production commandの起動失敗ログは固定failure classで記録する。Preview expiryとWorkspace idle scheduled handlerは内部DB errorをLambdaへ返さず固定再試行errorを返す。これは観測可能な秘密情報の削減であり、CloudWatch retention、alarm delivery、運用者アクセスの配備検証ではない。

## 残るリスク・受入条件

- **公開Webhookの負荷制御**: Tenant ingressではheader preflightの後、active integrationに限定したDSQL共有rate counterとAPI Gateway route throttleをHMAC/Secret取得より前に適用する（Task 68）。default-offのため、これらの制限はAWSへ配備されるまで有効にならない。429/DB failureの監視と実環境負荷の確認が残る。
- **trusted brokerの侵害範囲**: processのAssumeRole許可が複数Tenant readerに及ぶ場合、そのbroker侵害の影響も複数Tenantへ及び得る。IAM trust/KMS/Secret resource policyを実環境で確認し、用途とTenantを限定する。
- **実Runtime/通信経路**: MicroVM guest network/container権限、EFS Access Point越境、Gateway session ticketのreplay/revocation、agent相互認証は後続実装とAWS PoCの受入条件。通常Workspaceのポート認可はローカルで現行Service状態まで検証するが、実AWS上のControl Plane/Gateway通信は未確認。
- **運用**: 監査の保持/export、callback query非記録、readアクセス監査、Secret rotation、実AWSのDLQ redrive・backup/recovery演習、container image scanは未完了。手順は[Operations Runbook](operations-runbook.md)に記載し、Secret値を取得して確認しない。

## 再検証

```sh
go version
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
go vet ./...
go test ./...
HAKO_TEST_DATABASE_URL='postgres://…' go test -tags=integration ./... -count=1
```

CIにgovulncheckを追加。検出がある場合はCIを失敗させる。依存関係・toolchain更新だけで既存のAWS binaryが置き換わることはないため、承認されたrelease時に修正版artifactを再buildして配備する。
