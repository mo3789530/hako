ALTER TABLE workload_github_checks
    ADD COLUMN IF NOT EXISTS pipeline_name TEXT NOT NULL DEFAULT '',
    DROP CONSTRAINT IF EXISTS workload_github_checks_pkey,
    ADD CONSTRAINT workload_github_checks_pkey PRIMARY KEY (tenant_id, workload_run_id, pipeline_name),
    ADD CONSTRAINT workload_github_checks_pipeline_name_check
        CHECK (pipeline_name = '' OR pipeline_name ~ '^[a-z][a-z0-9_-]{0,39}$');
