CREATE INDEX IF NOT EXISTS github_webhook_deliveries_repository_pending_idx
    ON github_webhook_deliveries (received_at, delivery_id)
    WHERE processing_status = 'received'
      AND event_type = 'installation_repositories'
      AND installation_id IS NOT NULL;
