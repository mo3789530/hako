# Task 37: GitHub App Credentials and Installation Tokens

Status: Ready. Priority: P0. Depends on: none; parallel with Task 36.

## Goal

Enable the worker to authenticate as Hako's GitHub App without storing PATs, user OAuth tokens, or long-lived credentials in DSQL, SQS, job environments, logs, or artifacts. Existing code supports per-Tenant Installation setup via OAuth but does not mint Installation access tokens.

## Work

- Add configuration for App ID and a Secrets Manager private-key ARN; load the private key only in a narrowly scoped service/worker role.
- Implement short-lived App JWT signing and Installation token creation with the minimum repository permissions needed for read-only checkout. Add per-repository installation validation before token minting.
- Cache tokens only in process memory until shortly before expiry; never persist or include them in Workload metadata/message.
- Add bounded HTTP timeouts, retry classification, clock-skew handling, key rotation strategy, and safe error redaction.
- Do not grant write permissions for the initial Job checkout path.

## Acceptance criteria

- Unit tests cover JWT claims/expiry, token expiry/cache refresh, invalid Installation, revoked/suspended Installation, GitHub API errors, retries, and concurrent token requests.
- Integration tests use a fake GitHub API; tests never call public GitHub or require a real secret.
- Logs and returned errors cannot contain private-key bytes, JWTs, or Installation tokens.
- IAM is limited to `secretsmanager:GetSecretValue` on the configured App key secret; no AWS keys are introduced.
- Document rotation and emergency revocation procedure; update GitHub App minimum-permission docs.

## Constraints

Never echo secret values in tests/tool output. Do not implement push/comment/check permissions here; Checks API permissions are a separate task.
