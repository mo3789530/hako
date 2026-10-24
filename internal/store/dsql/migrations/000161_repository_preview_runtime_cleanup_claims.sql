ALTER TABLE repository_preview_runtime_allocations
    ADD COLUMN runtime_cleanup_attempts INTEGER NOT NULL DEFAULT 0 CHECK (runtime_cleanup_attempts >= 0),
    ADD COLUMN runtime_cleanup_claim_id TEXT,
    ADD COLUMN runtime_cleanup_lease_expires_at TIMESTAMPTZ,
    ADD COLUMN runtime_cleanup_next_attempt_at TIMESTAMPTZ,
    ADD COLUMN runtime_cleanup_last_error TEXT NOT NULL DEFAULT '',
    ADD CONSTRAINT repository_preview_runtime_cleanup_claim_check
        CHECK ((runtime_cleanup_claim_id IS NULL) = (runtime_cleanup_lease_expires_at IS NULL)),
    ADD CONSTRAINT repository_preview_runtime_cleanup_error_check
        CHECK (runtime_cleanup_last_error IN ('', 'preview_binding_missing', 'gateway_route_remove_failed', 'microvm_terminate_failed', 'provider_unavailable'));
