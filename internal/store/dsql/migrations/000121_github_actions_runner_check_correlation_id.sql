ALTER TABLE github_actions_runner_checks
    ADD COLUMN IF NOT EXISTS correlation_id TEXT CHECK (
        correlation_id IS NULL OR correlation_id ~ '^req_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    );
