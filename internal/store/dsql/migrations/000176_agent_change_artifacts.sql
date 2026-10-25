ALTER TABLE workload_artifacts
    DROP CONSTRAINT workload_artifacts_kind_check,
    ADD CONSTRAINT workload_artifacts_kind_check
    CHECK (kind IN ('logs', 'test_report', 'coverage', 'sbom', 'binary', 'container_image', 'agent_changes', 'other'));
