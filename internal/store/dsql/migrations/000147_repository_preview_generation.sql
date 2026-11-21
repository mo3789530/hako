ALTER TABLE repository_previews
    ADD COLUMN IF NOT EXISTS generation BIGINT NOT NULL DEFAULT 1 CHECK (generation > 0);
