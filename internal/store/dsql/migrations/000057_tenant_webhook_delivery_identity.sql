ALTER TABLE github_webhook_deliveries
    ADD COLUMN IF NOT EXISTS integration_id TEXT NOT NULL DEFAULT 'shared',
    DROP CONSTRAINT IF EXISTS github_webhook_deliveries_pkey,
    ADD CONSTRAINT github_webhook_deliveries_pkey PRIMARY KEY (integration_id, delivery_id);
