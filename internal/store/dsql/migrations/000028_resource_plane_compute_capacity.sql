ALTER TABLE resource_plane_capacities
    ADD COLUMN max_cpu_millicores BIGINT CHECK (max_cpu_millicores IS NULL OR max_cpu_millicores > 0),
    ADD COLUMN reserved_cpu_millicores BIGINT NOT NULL DEFAULT 0 CHECK (reserved_cpu_millicores >= 0),
    ADD COLUMN max_memory_mib BIGINT CHECK (max_memory_mib IS NULL OR max_memory_mib > 0),
    ADD COLUMN reserved_memory_mib BIGINT NOT NULL DEFAULT 0 CHECK (reserved_memory_mib >= 0)
