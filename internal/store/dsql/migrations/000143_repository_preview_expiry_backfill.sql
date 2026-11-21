WITH due AS (
    SELECT p.tenant_id, p.installation_id, p.github_repository_id, p.pull_request_number,
           COALESCE(q.preview_lifetime_seconds, 86400) AS lifetime_seconds
    FROM repository_previews p
    LEFT JOIN tenant_quota_limits q ON q.tenant_id = p.tenant_id
    WHERE p.desired_state = 'present' AND p.expires_at IS NULL
)
UPDATE repository_previews p
SET expires_at = p.created_at + due.lifetime_seconds * INTERVAL '1 second'
FROM due
WHERE p.tenant_id = due.tenant_id AND p.installation_id = due.installation_id
  AND p.github_repository_id = due.github_repository_id AND p.pull_request_number = due.pull_request_number;
