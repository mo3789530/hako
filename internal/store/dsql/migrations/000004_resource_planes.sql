CREATE TABLE IF NOT EXISTS resource_planes (
    id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    region TEXT NOT NULL,
    capabilities_json TEXT NOT NULL
);
