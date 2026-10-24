ALTER TABLE repository_previews
    DROP CONSTRAINT IF EXISTS repository_previews_state_check,
    ADD CONSTRAINT repository_previews_state_check
    CHECK (state IN ('awaiting_runtime', 'cleanup_pending', 'cleaned', 'quota_exceeded'));
