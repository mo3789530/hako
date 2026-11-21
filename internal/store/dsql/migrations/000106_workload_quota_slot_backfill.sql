INSERT INTO workload_quota_slots (tenant_id, slot, workload_run_id, created_at)
SELECT tenant_id, ROW_NUMBER() OVER (PARTITION BY tenant_id ORDER BY created_at, id)::INTEGER, id, created_at
FROM workload_runs
WHERE state NOT IN ('succeeded','failed','cancelled','timed_out')
ON CONFLICT DO NOTHING;
