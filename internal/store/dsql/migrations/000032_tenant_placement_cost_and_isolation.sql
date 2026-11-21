ALTER TABLE tenant_placement_policies
    ADD COLUMN max_cost_tier TEXT CHECK (max_cost_tier IS NULL OR max_cost_tier IN ('low', 'standard', 'high')),
    ADD COLUMN minimum_isolation_tier TEXT CHECK (minimum_isolation_tier IS NULL OR minimum_isolation_tier IN ('shared', 'dedicated', 'isolated'))
