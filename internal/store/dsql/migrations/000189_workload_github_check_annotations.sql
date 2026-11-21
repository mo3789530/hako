ALTER TABLE workload_github_checks
    ADD COLUMN IF NOT EXISTS output_annotations JSONB NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(output_annotations) = 'array');
