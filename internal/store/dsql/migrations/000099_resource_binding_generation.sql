ALTER TABLE resource_bindings
    ADD COLUMN IF NOT EXISTS allocation_generation BIGINT NOT NULL DEFAULT 0
    CHECK (allocation_generation >= 0);
