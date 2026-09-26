ALTER TABLE resource_planes
    ADD COLUMN cost_tier TEXT NOT NULL DEFAULT 'standard' CHECK (cost_tier IN ('low', 'standard', 'high')),
    ADD COLUMN isolation_tier TEXT NOT NULL DEFAULT 'shared' CHECK (isolation_tier IN ('shared', 'dedicated', 'isolated'))
