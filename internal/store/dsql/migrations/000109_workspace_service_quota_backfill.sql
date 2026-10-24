INSERT INTO workspace_service_quota_reservations (service_id, workspace_id, memory_bytes, milli_cpu, reserved_at)
SELECT id, workspace_id,
    (config_json::jsonb #>> '{resources,memory_bytes}')::BIGINT,
    (config_json::jsonb #>> '{resources,milli_cpu}')::BIGINT,
    updated_at
FROM services
WHERE desired_state = 'running' OR observed_state NOT IN ('stopped', 'deleted')
ON CONFLICT (service_id) DO NOTHING
