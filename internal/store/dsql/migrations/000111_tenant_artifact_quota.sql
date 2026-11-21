ALTER TABLE tenant_quota_limits
    ADD COLUMN IF NOT EXISTS artifact_bytes_limit BIGINT NOT NULL DEFAULT 10737418240
    CHECK (artifact_bytes_limit > 0 AND artifact_bytes_limit <= 107374182400);
