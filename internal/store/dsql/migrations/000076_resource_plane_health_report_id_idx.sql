CREATE UNIQUE INDEX IF NOT EXISTS resource_plane_health_events_report_id_idx
    ON resource_plane_health_events (resource_plane_id, report_id)
    WHERE report_id IS NOT NULL;
