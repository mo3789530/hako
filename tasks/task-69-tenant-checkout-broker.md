# Task 69: Tenant proof/checkout broker wiring

Status: Implemented locally (broker service/HTTP mTLS endpoint/client, DB-backed Run+credential pin resolver, and checkout provider routing verified). Production broker process/certificate provisioning and worker deployment wiring remain Tasks 40, 66, and 72. Depends on: 57, 38.

## Scope

Runのimmutable credential pinからtrusted brokerを呼び、Repository限定tokenと固定SHA checkoutを接続する。

既存の親タスクを分割した実行単位であり、追加の製品スコープではない。Lambda MicroVMを維持し、EC2は対象外。ローカルPostgresは停止しない。

## Acceptance criteria

- API/RPへApp private keyを渡さず、broker内部要求を認証する。
- Tenant/App/Installation/Repository/generation mismatchとdisable時に拒否し、shared fallbackしない。
- fake broker/GitHubとPostgresでrotation、失敗retry、既存shared Run互換をテストする。

## Handoff

- `internal/githubbroker/checkout.go` resolves a canonical persisted Workload Run and active exact credential pin, authorizes only the mTLS Control Plane identity, mints a short-lived repository-scoped Installation token, and rechecks authorization/Run/pin after GitHub I/O before returning it.
- `/v1/checkout/token` accepts only Tenant ID and Run ID; it rejects caller-supplied pins, returns `Cache-Control: no-store`, and never returns private-key material or AWS credentials. The Control Plane client refuses redirects and checks token expiry.
- `NewAWSCheckoutTokenMinter` composes the purpose-specific pinned private-key reader and live Tenant App/Repository authorizer. The private key remains inside the trusted broker; Workload Run secrets are not placed in queue messages.
- `repositorycheckout.NewGitRepositoryCheckoutWithRunCredentials` routes owned Runs to the mTLS broker and keeps legacy shared Runs on their existing shared provider. An owned Run cannot fall back when the broker is missing or fails.
- Tests cover mTLS HTTP request/response, replayable transient mint retry, Run cancellation/disable/rotation fencing, exact active pin and cross-Tenant denial in isolated PostgreSQL, plus shared/owned provider routing. Existing GitHub App tests cover Installation and Repository mismatch.
- Full `go test ./...`, full `go test -tags=integration ./... -count=1`, and `go vet ./...` pass. Integration tests used the running local PostgreSQL and isolated schemas.
- Remaining deployment work: broker server composition/certificate lifecycle, exact IAM provisioning, and workload worker configuration. No AWS apply, real Secret retrieval, or external Tenant cutover was performed.

- 実AWS apply、実Secret取得、外部設定変更はこのタスクのローカル実装に含めない。
