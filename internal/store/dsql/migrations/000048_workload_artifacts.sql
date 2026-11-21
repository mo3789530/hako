CREATE TABLE IF NOT EXISTS workload_artifacts (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    workload_run_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('logs', 'test_report', 'coverage', 'sbom', 'binary', 'container_image', 'other')),
    storage_ref TEXT NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    sha256 TEXT NOT NULL,
    redacted BOOLEAN NOT NULL DEFAULT FALSE,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (tenant_id, workload_run_id) REFERENCES workload_runs(tenant_id, id),
    CHECK (kind <> 'logs' OR redacted = TRUE)
)
