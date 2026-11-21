ALTER TABLE github_webhook_deliveries
    ADD CONSTRAINT github_webhook_agent_claim_pair_check
    CHECK ((agent_processing_claim_id IS NULL) = (agent_processing_lease_expires_at IS NULL));
