CREATE INDEX IF NOT EXISTS github_actions_runner_cleanup_claim_idx
    ON github_actions_runner_cleanup_outbox(state, available_at, created_at);
