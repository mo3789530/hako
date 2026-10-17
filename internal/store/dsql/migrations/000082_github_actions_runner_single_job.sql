ALTER TABLE github_actions_runner_jobs
    ADD CONSTRAINT github_actions_runner_jobs_runner_single_job
    UNIQUE (integration_id, installation_id, repository_id, runner_id);
