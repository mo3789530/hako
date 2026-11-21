CREATE TABLE IF NOT EXISTS runtime_class_resources (
    runtime_class TEXT PRIMARY KEY CHECK (runtime_class <> '' AND runtime_class = LOWER(BTRIM(runtime_class))),
    cpu_millicores BIGINT NOT NULL CHECK (cpu_millicores > 0),
    memory_mib BIGINT NOT NULL CHECK (memory_mib > 0),
    updated_at TIMESTAMPTZ NOT NULL
)
