ALTER TABLE tenant_quota_limits
    ADD COLUMN IF NOT EXISTS preview_run_limit SMALLINT NOT NULL DEFAULT 2
        CHECK (preview_run_limit > 0 AND preview_run_limit <= 5),
    ADD COLUMN IF NOT EXISTS preview_lifetime_seconds BIGINT NOT NULL DEFAULT 86400
        CHECK (preview_lifetime_seconds >= 300 AND preview_lifetime_seconds <= 86400);
