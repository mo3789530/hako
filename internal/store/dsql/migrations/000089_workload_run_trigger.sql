ALTER TABLE workload_runs
    ADD COLUMN IF NOT EXISTS trigger_event TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS trigger_branch TEXT NOT NULL DEFAULT '',
    ADD CONSTRAINT workload_runs_trigger_pair_check CHECK (
        (trigger_event = '' AND trigger_branch = '') OR
        (trigger_event IN ('push', 'pull_request') AND trigger_branch <> '')
    );
