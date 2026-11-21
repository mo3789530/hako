ALTER TABLE workspace_status
    ADD COLUMN IF NOT EXISTS gateway_admission_window_started_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS gateway_admission_count INTEGER NOT NULL DEFAULT 0;
