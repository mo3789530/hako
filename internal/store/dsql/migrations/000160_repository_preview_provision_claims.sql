ALTER TABLE repository_preview_runtime_allocations
    ADD COLUMN provision_attempts INTEGER NOT NULL DEFAULT 0 CHECK (provision_attempts >= 0),
    ADD COLUMN provision_claim_id TEXT,
    ADD COLUMN provision_lease_expires_at TIMESTAMPTZ,
    ADD COLUMN provision_next_attempt_at TIMESTAMPTZ,
    ADD COLUMN provision_last_error TEXT NOT NULL DEFAULT '',
    ADD CONSTRAINT repository_preview_runtime_provision_claim_check
        CHECK ((provision_claim_id IS NULL) = (provision_lease_expires_at IS NULL)),
    ADD CONSTRAINT repository_preview_runtime_provision_error_check
        CHECK (provision_last_error IN ('', 'runtime_profile_missing', 'microvm_launch_failed', 'gateway_route_publish_failed', 'provider_unavailable'))
