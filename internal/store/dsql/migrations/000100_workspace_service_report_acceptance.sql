ALTER TABLE workspace_service_report_events
    ADD COLUMN IF NOT EXISTS accepted BOOLEAN NOT NULL DEFAULT FALSE;
