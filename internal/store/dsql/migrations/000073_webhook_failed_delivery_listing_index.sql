CREATE INDEX IF NOT EXISTS github_webhook_failed_delivery_listing_idx
    ON github_webhook_deliveries (integration_id, received_at DESC, delivery_id)
    WHERE processing_status = 'failed'
