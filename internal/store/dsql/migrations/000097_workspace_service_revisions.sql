ALTER TABLE services
    ADD COLUMN IF NOT EXISTS desired_revision BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS observed_revision BIGINT NOT NULL DEFAULT 0,
    ADD CONSTRAINT services_revision_bounds_check
    CHECK (desired_revision >= 1 AND observed_revision >= 0 AND observed_revision <= desired_revision);
