UPDATE github_webhook_deliveries
SET payload_expires_at = received_at + INTERVAL '7 days'
WHERE payload_expires_at IS NULL
