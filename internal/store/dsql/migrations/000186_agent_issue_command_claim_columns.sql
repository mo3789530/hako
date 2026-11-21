ALTER TABLE github_webhook_deliveries
    ADD COLUMN agent_processing_claim_id TEXT,
    ADD COLUMN agent_processing_lease_expires_at TIMESTAMPTZ;
