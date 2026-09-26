# GitHub App Installation setup

The Installation ID on GitHub's setup redirect is untrusted input. Hako does
not activate a Tenant claim based on that ID alone. The opt-in setup flow
correlates a Hako Owner/Admin request with a short-lived state, obtains a
GitHub App user access token through OAuth, verifies the requested Installation
is visible to that GitHub user and belongs to the configured Hako App, then
rechecks Hako Tenant authorization before binding it.

## Flow

1. A Tenant Owner/Admin calls
   `POST /v1/tenants/{tenant_id}/github/installations/setup` with `{}`. Hako
   creates a random 256-bit state, stores only its SHA-256 hash with the Tenant,
   Hako user, PKCE verifier, and 10-minute expiry, and returns the App
   installation URL. The verifier is used to create an OAuth S256 challenge;
   it is never sent to the browser.
2. GitHub redirects to Hako's Setup URL with `installation_id`, `setup_action`,
   and `state`. Hako attaches the candidate ID to the still-valid state and
   redirects to GitHub OAuth authorization with the same state.
3. GitHub redirects to the App OAuth callback URL with `code` and `state`.
   Hako exchanges the one-time code using the App OAuth client secret and PKCE
   verifier, then
   calls `GET /user/installations` with the transient user token. It paginates
   the user-visible Installations and checks the candidate ID and configured
   App ID; it does not trust the setup redirect's ID or account name.
4. In one DSQL transaction, Hako consumes the state, rechecks that the Hako
   user is still a Tenant Owner/Admin, creates the active Installation claim,
   and inserts the globally unique Tenant binding. A state replay, expired
   state, removed Hako membership, or Installation already bound to another
   Tenant cannot activate a binding.

The user access token is used only for the GitHub verification request and is
never returned to the browser, logged, or persisted. App installation tokens
and App private-key management are separate, not yet implemented features.
The PKCE verifier is stored in the Control Plane database only for the short
setup window; successful completion erases it, and expired/consumed setup rows
are reaped when a new setup starts. Configure API Gateway/application access
logs not to record callback query strings: OAuth `code` and `state` arrive in
the callback URL, and credentials must not be retained in logs.

## GitHub App and Terraform configuration

In the GitHub App settings, configure both the Setup URL and OAuth Callback URL
to the Control Plane route:

```text
https://<api-host>/v1/integrations/github/setup/callback
```

Set these Control Plane Terraform inputs:

- `github_app_setup_enabled = true`
- `github_app_slug` — GitHub App URL slug (not its display name)
- `github_app_id` — numeric App ID (not OAuth client ID)
- `github_app_client_id`
- `github_app_client_secret_arn` — Secrets Manager ARN with a non-empty
  `SecretString`
- `github_app_callback_url` — the exact HTTPS OAuth callback URL registered
  above

The API role gets `secretsmanager:GetSecretValue` only for this client-secret
ARN. The OAuth route remains disabled unless setup is explicitly enabled. The
OAuth client secret is loaded at API startup; rotate it by updating the
SecretString and redeploying/restarting the API. The App's separate private
key is not required by this user-verification flow.

After migration `000043`, run the SQL bootstrap grant for
`github_installation_setup_states` as described in
[`control-plane-terraform.md`](control-plane-terraform.md). Terraform apply,
GitHub App settings changes, and secret creation are operator actions and are
not performed by local tests.

## API examples

```sh
curl -X POST "$HAKO_API/v1/tenants/$TENANT_ID/github/installations/setup" \
  -H "Authorization: Bearer $HAKO_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{}'
```

Open the returned `setup_url` in the browser. The final callback reports the
verified active Installation; the existing manual claim endpoint intentionally
continues to create only inert `pending` claims.

GitHub reference: [setup URLs](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/about-the-setup-url),
[user access tokens](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app),
and [GitHub App Installation REST endpoints](https://docs.github.com/en/rest/apps/installations).
