ALTER TABLE workload_runs
    DROP CONSTRAINT IF EXISTS workload_runs_trigger_pair_check,
    ADD CONSTRAINT workload_runs_trigger_pair_check CHECK (
        (trigger_event = '' AND trigger_branch = '') OR
        (trigger_event IN ('push', 'pull_request', 'issue_comment') AND trigger_branch <> '')
    );
