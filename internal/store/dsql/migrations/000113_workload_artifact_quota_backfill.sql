INSERT INTO workload_artifact_quota_reservations (tenant_id, workload_run_id, reserved_bytes, created_at)
SELECT tenant_id, id, 104857600, created_at
FROM workload_runs
WHERE state NOT IN ('succeeded','failed','cancelled','timed_out')
ON CONFLICT (tenant_id, workload_run_id) DO NOTHING;
