ALTER TABLE github_actions_runner_jobs
    ADD COLUMN runner_id BIGINT CHECK (runner_id IS NULL OR runner_id > 0),
    ADD COLUMN runner_name TEXT NOT NULL DEFAULT '',
    ADD COLUMN conclusion TEXT NOT NULL DEFAULT '' CHECK (length(conclusion) <= 100),
    ADD COLUMN completed_at TIMESTAMPTZ;
