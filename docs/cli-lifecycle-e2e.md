# Authenticated CLI Lifecycle E2E

`TestAuthenticatedCLIWorkspaceLifecycleThroughFakeRuntime` exercises the CLI authentication and Workspace client path against a real PostgreSQL schema, Echo API, Fake Runtime, and Operation Result Consumer.

## Scenarios

1. Run the CLI's Authorization Code + PKCE login function against a local OAuth test server. The authorization callback validates state, exchanges the code, and stores the access token in an in-memory credential store implementing the same interface as the OS keyring.
2. Use the saved token through the CLI's API request helpers to create and list a Workspace.
3. Read the committed command from the Workspace Operation's Outbox record, execute it in Fake Runtime, and pass its result through the Control Plane result consumer.
4. Verify the Workspace through the authenticated `get` path, then suspend, resume, and delete it in sequence. Verify final Desired/Observed State and quota release.
5. Redeliver the same create command and verify Fake Runtime applies it once. Simulate result-queue ack failure after DB commit, verify the redelivery is a terminal-state no-op, and confirm only one terminal Operation event exists.
6. Verify a Tenant Member cannot get or suspend another Member's Workspace; both are returned as the same `404 not_found`. For lifecycle actions, assert Desired State changes when accepted while Observed State remains unchanged until the Fake Runtime result is consumed.

The test uses a synthetic RS256 token and local JWKS/OAuth endpoints. PostgreSQL is real and isolated by a random schema; the API, authentication, database transitions, and CLI API client are exercised. The command transport is read from Outbox directly and the result queue is in memory with deterministic duplicate/ack-failure behavior, so the test does not cover separate Dispatcher/Controller/Consumer processes, real SQS redrive/DLQ, the OS keyring, or a real Cognito User Pool. Those remain AWS/process-level E2E coverage.

## Run

```sh
make test-integration-local
```

Or set `HAKO_TEST_DATABASE_URL` to a disposable PostgreSQL database and run:

```sh
go test -tags=integration ./cmd/hako ./internal/api ./internal/integration -count=1
```

The integration helper creates a random schema and removes only that schema on completion. It never drops the database or a Compose volume. No AWS account, Cognito domain, or keyring daemon is needed.
