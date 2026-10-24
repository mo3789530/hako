# Lambda MicroVM Workload Controller

HakoのWorkload基盤はAWS Lambda MicroVMs。`internal/workloadcontroller`は条件付きRP journal、trusted specification resolver、MicroVM provider、result publisherを注入して使う。RPからDSQLへ接続しない。

## 実装済みの経路（Tasks 63–65）

1. `Ensure`でidentity-only commandを受理し、固定commit/source/image digest・deadline・placementの仕様metadataとハッシュをCASで保存する。Secret値は含めない。cleanupはこのmetadataで回復し、Repository認可サービスの存続に依存しない。
2. `LambdaMicroVM`が公式SDKの`RunMicrovm`へ固定ClientToken、固定image/version、固定network connector、固定maximum durationを渡す。応答不明時も同じparametersを使う。
3. MicroVM IDをCASで保存する。`GetMicrovm`のstateだけでJob開始済みと判断せず、注入した`AgentRunner.Start`で準備と一度だけのJob開始を確認する。Agent接続時は`CreateMicrovmAuthToken`で当該MicroVM・TCP port 8080だけ・5分の認証tokenを発行し、runnerへ一時的に渡す。AWS endpointは認証後に`X-aws-proxy-*`をAgentへ転送しないため、別途VM/attempt/operationへ束縛した5分Ed25519 ticketを`Authorization` headerで送る。SDK stateはeventually consistent。
4. `Poll`のstatus/result取得にも操作ごとに新しい同スコープtokenを発行する。Tokenはjournal・DB・RunHookPayload・ログへ保存しない。`Poll`がagentのJob status/resultを取得し、Tenant/Run/attempt/generation/commit、redacted logのサイズ・digest・attempt別S3 prefixを検証する。結果metadataをjournalへ保存してから送信し、送信後checkpoint失敗時は同じ結果を再送する。
5. `Cancel`はjournalへtombstoneとlogical terminal stateを保存してから停止する。`TerminateMicrovm`成功だけではcleanup完了とせず、`GetMicrovm`の`TERMINATED`を確認する。停止/cleanup失敗は再試行できる。

generation > 1のSpecificationには直前attemptのidentityが必要。同じTenant/Run/Resource Planeの直前generationについてcleanup確認済みのjournalを読み取れなければreplacementを起動しない。

CPの`RequestCancel`はdesired stateとcurrent attempt向け`workload_attempt.cancel` Outboxを同じtransactionに保存する。Dispatcherは固定Resource Plane queueへ配送し、`HakoEventType`でdispatch/cancelを識別する。再度のcancelでは追加Outboxを作らない。attempt作成前のcancelはschedulerが検出する。既存DB roleにはbootstrap templateの`SELECT ON workload_run_attempts`を適用する必要がある。

## AWSの仕様とHakoの制約

Resource Planeの`HAKO_WORKLOAD_RUNTIME_PROFILES_JSON`はWorkload kind、Runtime
Class、Trust Levelごとに固定イメージ/Network Connectorと実行上限を定義する。
`kind=job`の各profileには`job_memory_limit_mb`（256–16384）と
`job_cpu_limit_millis`（250–8000）が必須で、これはGuest OS/containerd等の
オーバーヘッドを差し引いたJobへ割当可能な上限としてOperatorが設定する。
Control Plane APIやRepository manifestからこの値を指定できない。Controllerは
値をMicroVM Run HookとEd25519 Start/Source Ticketへ束縛し、Guestはmanifestの
pipeline全要求がこの上限内であることを検証してから実行する。値はMicroVMの
実測可能容量やcgroup適用成功を証明するものではなく、AWSでのPoCとcgroup v2
enforcement検証は別途必要である。

- **1 MicroVMの最大存続時間は8時間（28,800秒）、ARM64のみ。** 汎用Run APIのdeadline上限24時間と異なる。AWS adapterは元のrequest intervalが8時間を超えるSpecificationを起動前に拒否する。fake-clockの24時間テストはCP契約の検証であり、AWSで24時間実行できる証明ではない。
- **Resource quotaとAPI throughputもPlacement/Dispatcherの前提にする。** 公開Quota表ではMicroVM baseline memory合計は通常のaccount/Regionで400 GiB、Tokyoを含む一部Regionでは1,024 GiBで、最大4倍までburst可能。MicroVM一台ごとの非調整可能な最大durationは8時間。`RunMicrovm`は5 TPS / burst 5、`GetMicrovm`は100 TPS、`CreateMicrovmAuthToken`は50 TPS、`TerminateMicrovm`は10 TPS（いずれもaccount/Region単位）。既定値は変更され得るため、運用開始時に対象account/RegionのService Quotasを確認し、Scheduler側でRun起動を平滑化、AWS throttlingはexponential backoff+jitterで再試行する。HakoはまだQuotaをAWSから同期せず、DBのResource Plane capacity設定は実quotaの証拠ではない。
- **Runtime classは公開サイズ表に合せる。** baseline 0.5/1/2/4/8 GiBはそれぞれ0.25/0.5/1/2/4 vCPU、peakは最大4倍。最大diskは8/8/8/16/32 GiB。Hakoの`standard`案（4 GiB/2 vCPU baseline、16 GiB/8 vCPU peak、16 GiB disk）は公開サイズの一つに合致する。diskサイズはimage作成時の設定を検証するまでHako側の保証値として扱わない。
- **接続数にも上限がある。** 公開値は1/2/4/8/16 vCPUに対して8/16/32/64/128 concurrent connections per MicroVM。HakoのSSH/VS Code/shell/port-forwardとAgent接続の合算設計に影響するため、1 Workspace/JobあたりのGateway同時stream数・並行local forwardを実PoCで測り、上限に近づく前に拒否またはbackpressureする。API上のlocal capだけではAWS側throughputの検証にならない。
- **Lambda MicroVM imageのDockerfile buildとnested containerは別の機能。** AWSはzip内DockerfileをMicroVM build環境で実行し、アプリケーションcontainerと初期snapshotを作る。これはMicroVM内でDocker/containerd/Podman daemonを動かせる証拠ではない。公開資料では標準Linux capabilitiesに加え`additionalOsCapabilities=["ALL"]`を選べると説明されているが、Hakoはそれを付与していない。rootless containerd/Podman、Compose、namespace/cgroup/resource limits、Tenant間隔離はimage builderと実MicroVMで個別に検証する。`ALL`を使う場合もVM内権限の影響と攻撃面をレビューし、未検証のまま有効化しない。
- Lambda管理MicroVM base imageはAmazon Linux 2023を提供し、Dockerfileのcontainer base imageとは別resource。managed base imageにはdeprecation/expiry/revocation lifecycleがある。Releaseはimage buildに使ったbase image version、Dockerfile base、artifact digestを記録し、base image更新後のbuild/validateを通してからactive versionを切り替える。
- deadlineは作成時基準の絶対時刻。AWS maximum durationはretryで変化させず、agentも絶対deadlineを守り、controllerが期限切れを停止する。期限延長・自動Job再実行・暗黙の別基盤への変更はしない。
- Jobのidle policyはauto-resume無効、max idleを固定maximum duration以上（最小60秒）に設定する。proxyへのinbound通信量で判定される標準idle policyにより、無通信の長時間Jobが途中suspendすることを避ける。
- Lambda MicroVMsの標準egressはInternet。承認済みVPC egress connectorを必須とし、VPC側でdefault-denyと必要経路だけを設定する。connector ARN指定だけでネットワーク隔離済みとは扱わない。
- VPC egress connectorはMicroVMのoutboundをVPCへ接続する機能であり、それ自体がNATではない。VPC内宛てやVPC endpoint宛てはVPC内経路を使い、Internet宛てIPv4通信がNAT Gatewayを通るのはconnectorのENIが使うsubnetのroute tableにdefault route（`0.0.0.0/0 -> nat-*`）がある場合。NAT Gatewayのないroute tableならInternetへ出られず、connectorなしならLambda MicroVMの標準Internet egressとなる。Hakoは現在connector/NAT/FirewallをTerraform配備していないため、実経路は未確定。
- NAT Gatewayは送信元変換と戻り通信を提供するが、宛先FQDN単位のallowlistやegress filteringではない。Software Factoryのuntrusted jobには、VPC routeをNATへ向けるだけでなく、Network Firewall/egress proxy等でdefault-denyと必要な宛先制御を別途設計・検証する。
- Native Job imageはAgent listenerを開く前に、Job UID 65532とrootless containerdのdaemon/mapped UID rangeについてIPv4/IPv6のloopback-only OUTPUT guardを適用する。適用失敗時はAgent起動を拒否し、Job開始時にも再適用する。これはMicroVM内のhost process境界であり、外側のVPC connector/firewall、container network、netfilter/owner-matchのtarget kernel supportを代替・証明しない。これらの実MicroVM PoCが完了するまでService manifestとWorkload schedulingはdefault-offを維持する。
- **Worker LambdaのVPC接続はMicroVM egress connectorとは別物。** opt-in Terraform deploymentにはResource Planeが所有する2つ以上のprivate subnet IDと専用security group IDを必須にする。Lambda execution roleのENI作成は入力されたsubnet/SG ARNとENI resourceに限定し、assign/delete/unassignはENI resource、wildcardはread-only `DescribeNetworkInterfaces`だけにする。Subnets/route tables/endpoints/NAT/firewall/SG egress rules自体はこのmoduleで作成せず、Lambda、SQS、S3、Secrets Manager、KMS、およびControl Plane/broker endpointへの必要経路をResource Plane側で用意する。private subnetのLambdaからpublic HTTPS endpointへ出る場合はNAT等の経路が必要で、AWSサービス向けInterface/Gateway VPC endpointを構成する場合はNATなしで接続できる。接続先・DNS・security group reachabilityは配備前に別途検証する（[Lambda VPCインターネット接続](https://docs.aws.amazon.com/lambda/latest/dg/configuration-vpc-internet.html)、[Lambda API VPC endpoint](https://docs.aws.amazon.com/lambda/latest/dg/configuration-vpc-endpoints.html)、[EC2 ENI IAM resource scope](https://docs.aws.amazon.com/service-authorization/latest/reference/list_ec2.html)）。
- 初期adapterはcommercial `aws` partition、固定image ARN/numeric version/digest、明示的なAWS-managed `ALL_INGRESS` connectorを扱う。ControllerはHTTPS接続を検査し、`CreateMicrovmAuthToken`のscopeをagent port 8080だけ・5分に制限してrunnerへ渡す。接続のtoken issuance境界、署名付きAgent ticket、protocol v1 HTTP client/handler、Controller adapter、固定`go test ./...` process backend、および`cmd/hako-agent` listener executableを実装済み。Agentは固定port 8080にbindし、image-ownedの公開Ed25519鍵だけを読み込み、終了時に子processを停止する。Local ARM64 image artifact packagingとready/validate/run hook handlerも追加したが、実MicroVM image builder/hook behavior、公開鍵の配布・rotation、network isolationは未検証。公開shell connectorは追加しない。
- Agent protocol v1 uses separate `PUT .../source` and `GET .../logs` operations with five-minute VM/attempt-scoped tickets. Start/Source tickets cryptographically bind source SHA-256, immutable commit SHA, and absolute deadline as well as VM/attempt/operation; the Agent requires upload headers and Start body to match those claims exactly. The Controller streams the content-addressed tar from the exact attempt artifact prefix; the Agent verifies and safely extracts it, then records full attempt, digest, and commit binding outside the source tree. The fixed Go-test backend re-archives the extracted tree and checks its digest immediately before execution, so changed files fail closed. Completed Agent logs are redacted, bounded in memory, and exposed only through the operation-scoped logs ticket; the client verifies content type, size, and SHA-256. The Resource Plane Controller then writes the verified logs to the attempt-scoped S3 prefix using its own IAM role. The MicroVM does not receive S3 credentials or write directly to AWS. `NewFilesystemAgent` composes source installer, signed-claim/fixed-path Resolver, ProcessBackend, and HTTP handler; `cmd/hako-agent` serves it on port 8080 with bounded image-owned public-key loading and graceful shutdown. The local image artifact includes AWS `/ready`, `/validate`, and strict `/run` hooks; `/run` pins provider MicroVM ID, exact attempt, source digest, commit, and deadline before accepting protocol traffic. Actual Lambda image-builder/run-hook behavior, public-key distribution, and AWS PoC remain unverified; see [Workload Agent MicroVM Image](workload-agent-image.md).
- `ExecutionRoleArn`は指定しない。取得したMicroVMに想定外のexecution roleがあればagentを開始しない。GitHub App private keyやSecret値を`runHookPayload`へ渡さない。
- `/run` hookはtrusted agentの初期化だけを行う。unknown launchのcancel時に、同じClientTokenでIDを回復する`RunMicrovm`が新しいVMを作る可能性があるため、hookでrepository codeを実行してはならない。
- Workload Job adapterではsuspend/resumeを使わない。Workspace向けAPI operationは存在するが、EFS再作成・Process recovery・8時間上限を含むWorkspace契約は[別文書](workspace-lambda-microvm.md)に従い、実AWS PoCは未実施。

`NewLambdaWorkloadController`はWorkerとRecovery Lambdaで共有するRuntime/Agent/Resolver構成を作り、`NewLambdaWorkloadRecoveryHandler`は同じ構成にbounded recovery lister、lease manager、result publisherを接続します。Recovery対応journalを実装していない、またはlease/publisher/sweep limitが欠落した場合はcompositionを拒否します。これはローカルDI compositionの検証であり、AWS SDK client・Secret/key・mTLS fileの読み込みやEventBridge配備は含みません。

`cmd/hako-workload-controller-lambda`は`command`/`recovery`個別modeに加えて`combined` modeを持ちます。combinedではSQS `Records`イベントだけをcommand handlerへ、`source=aws.events`かつ`Records`なしのschedule envelopeだけをrecovery handlerへ渡し、それ以外・曖昧なevent shapeは拒否します。これにより同一のResource Plane専用functionをSQS Event Source Mappingとbounded recovery scheduleの双方へ接続できます。Terraformでのfunction・trigger配備はまだ追加されておらず、local event dispatch testはAWS invocationやIAM/queue visibilityの実証ではありません。

`SQSResultPublisher`はvalidated Workload resultを専用HTTPS SQS queueへ送ります。FIFOの場合はTenant/Runごとの安定Group IDとpayload hashによるDeduplication IDを設定します。既存Workspace Operation result queueと共有してはいけません。専用Workload result queue、Control Plane consumer binding、IAM、DLQ/alarmsは未配備です。
- Docker/containerd/Podman、EFS、imageの署名・source搬送の実証はTask 61/43–45に残る。一般Lambda functionのcontainer image配備とは異なるAPIを使う。

`RepositorySourceMaterializer`はControl Plane metadataを既存のbroker-aware immutable checkoutに渡し、checkout pathとattempt identityを`SourceArchiveStager`へ接続します。Tenant-owned Run向けの`NewTenantBrokeredRepositorySourceMaterializer`は、HMAC認証済みControl Plane metadataだけからrepository authorizerを構成し、immutable Tenant/Run pinをmTLS checkout brokerへ渡します。Tenant pinがないRunは拒否し、shared credential providerへfallbackしません。`workloadsource`は`.git`/symlink/special fileを除外または拒否する決定的tar、SHA-256照合付きのbounded extractor、SSE-S3/SSE-KMSを使うattempt/generation-scoped S3 stagerを実装しています。Controller→Agentの認証付きsource転送とAgent内install、およびAgent→Controllerの認証付きredacted log回収も実装済みです。Workload LambdaはResource Plane専用の5つの具体的Secrets Manager ARNからmetadata HMAC key、broker mTLS certificate/key/CA、Agent ticket private keyを起動時に読み込みます。secret responseはサイズ上限を持ち、秘密値をerror/logへ出さず、一時byte sliceはparse後に消去します。production roleはこの5 ARNだけに`secretsmanager:GetSecretValue`を許可する必要があり、secret rotation時のLambda実行環境更新/再起動手順は配備runbookのゲートです。テストはsynthetic fake APIのみを使い、実Secretを取得しません。Resource PlaneごとのmTLS SPIFFE identityはcheckout endpointだけに許可し、broker側の明示allowlistを要求します。Resource Plane Terraformには専用キューroleへアタッチするdefault-off runtime policyを追加し、1つの固定MicroVM image ARN、journal/source/artifactの個別prefix、exact recovery lease object、任意のexact KMS keyを対象にします。AWS IAMがResourceを指定できない`lambda:PassNetworkConnector`/`lambda:ConnectMicrovm`は`Resource: "*"`を要するため、専用RP roleへ隔離し固定profileでconnectorを制限します。これはLambda実行関数/VPC、queue mapping、S3 bucket/KMS key policyの配備ではなく、live permissionの証明でもありません。`make build-workload-controller-lambda`はLambda ARM64 ZIPを作成します。AWS API action/resource scopeは[AWS Lambda Service Authorization Reference](https://docs.aws.amazon.com/service-authorization/latest/reference/list_lambda.html)、MicroVM IAM action概要は[AWS Lambda MicroVM security documentation](https://docs.aws.amazon.com/lambda/latest/dg/microvms-security.html)を参照してください。

## 未接続の本番構成

controller、公式SDK adapter、CP cancel Outbox、result replayのlibraryは実装済み。Resource PlaneにはSQS partial-batch adapterと、prefix-scoped S3 journal listingを使うbounded recovery sweep primitive（ページ/失敗件数上限、attempt単位の失敗隔離、continuation loop拒否）がある。追加でS3 conditional objectを使ったrecovery lease claim/renew/expiry takeover/releaseと、lease-loss時にcancelするsweep orchestrationをローカル実装・テスト済み。Agent向けAWS token + VM/attempt/operation限定Ed25519 ticket、protocol v1 HTTP client/handler/Controller adapter、固定Go test process backend、Controllerが回収したログをattempt prefixへ書くS3 writerも実装済み。MicroVM側はAWS credentialを持たず、redacted logをticket認証付きendpointでResource Plane Controllerへ返す。さらに依存注入型の`NewLambdaWorkloadHandler`がSQS dispatchからjournal、trusted resolver、Lambda MicroVM、source/log adapter、Agent実行を合成し、fake end-to-endでsource install、result/log回収・保存、publisher通知、cleanup確認まで検証する。foreign-plane commandはprovider API呼出し前に拒否される。これは実行binary/SDKクライアントやproduction key/IAM wiringの証明ではない。source checkout→決定的tar→attempt別S3→Agentのbounded upload→digest検証・安全な展開→commit binding→実行直前のtree再検証もローカル実装・テスト済み。Control Planeには別HMAC keyで署名するdefault-off execution-metadata GET APIを追加し、DBからTenant/Run/attempt/latest generation/Placement/Installation/Repository/active Tenant App pinを再検証して非Secret metadataだけを返す。Resource Planeごとの専用Secret ARN mapが空ならrouteは無効であり、health-report keyとの再利用もTerraformで拒否する。Resource Planeのmetadata HTTP clientも実装し、同じcanonical HMAC、HTTPS、identity/deadline照合、redirect/proxy無効化、response上限を検証する。APIはRunの`created_at`を`requested_at`として返し、RPは固定された実行interval（最大24時間）を検査する。これらをResource Plane所有の`workload_kind × runtime_class × trust_level` profileとidempotentなsource materializer契約に合成する`MetadataSpecificationResolver`もローカル実装・テスト済み。Job用profileをAgent/Previewへ流用せず、profile未定義のkindはsource checkoutより前に拒否する。APIの署名は`GET\n<escaped-path>\n<unix-timestamp>\n<request-id>\n<hex(SHA256(empty))>`、headerは`X-Hako-Timestamp`、`X-Hako-Request-ID`、`X-Hako-Signature`。Local MicroVM artifact packagingと`/run` hookによるVM/attempt/execution bindingもテスト済み。実Lambda image build/run-hook behavior、production signer-key/Secret composition、public-key image rotation、worker queue/IAM/SQS配備、journal recovery scheduler、image/network PoCは未完了。Leaseは正確な単一Object ARNだけに権限を限定する必要がある。Go testsは任意のネットワーク通信を行えるため、default-deny egressの実証までWorkload作成はdefault-off。continuation tokenは単一invocation内だけで使い、scheduled sweepは毎回先頭から再走査する。durable journal保存だけでmessageをackするworkerを配備しない。

実AWSのclient-token再試行・保持期間、並行launch/terminate、agent起動、connectorのegress制限、quota/費用、cleanup/孤立VMの回収とalarmをTask 40で検証するまでRun作成はdefault-off。今回AWS APIを実行してリソースを作成・変更していない。

## 検証

Tasks 63–65のunit/race testsはfake journal/providerとSDK stubを使う。PostgreSQL integrationはCP cancellation transactionのrollback/duplicate、cancelがdispatchより先に届くケース、RP resultのCP commit後ack喪失・duplicate replay・artifact metadata一度だけ保存を検証する。常時起動DBは停止しない。

```bash
make test-workload-runtime
make test-workload-runtime-integration
```

integrationは既定のローカルPostgresまたは`HAKO_TEST_DATABASE_URL`を使い、テストごとのschemaを作成・回収する。実AWSリソースやDBコンテナのlifecycleは操作しない。

## 公式資料

- [Lambda MicroVMs](https://docs.aws.amazon.com/lambda/latest/dg/lambda-microvms-guide.html)
- [Quotas](https://docs.aws.amazon.com/lambda/latest/dg/gettingstarted-limits.html)
- [RunMicrovm](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_RunMicrovm.html)
- [GetMicrovm](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_GetMicrovm.html)
- [CreateMicrovmAuthToken](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_CreateMicrovmAuthToken.html)
- [TerminateMicrovm](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_TerminateMicrovm.html)
- [Networking](https://docs.aws.amazon.com/lambda/latest/dg/microvms-networking.html)
- [CreateMicrovmAuthToken](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_CreateMicrovmAuthToken.html)
- [Security/IAM](https://docs.aws.amazon.com/lambda/latest/dg/microvms-security.html)
- [IdlePolicy](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_IdlePolicy.html)
- [MicroVM images and supported sizes/build hooks](https://docs.aws.amazon.com/lambda/latest/dg/microvms-images.html)
- [Lambda MicroVM quotas](https://docs.aws.amazon.com/lambda/latest/dg/gettingstarted-limits.html)
