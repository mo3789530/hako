ALTER TABLE resource_plane_reservations
    ADD COLUMN runtime_class TEXT,
    ADD COLUMN cpu_millicores BIGINT CHECK (cpu_millicores IS NULL OR cpu_millicores > 0),
    ADD COLUMN memory_mib BIGINT CHECK (memory_mib IS NULL OR memory_mib > 0)
