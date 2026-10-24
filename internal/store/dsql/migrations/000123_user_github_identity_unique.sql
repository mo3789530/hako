CREATE UNIQUE INDEX IF NOT EXISTS users_github_user_id_uidx
    ON users (github_user_id);
