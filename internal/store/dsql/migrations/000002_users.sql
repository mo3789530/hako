CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    cognito_subject TEXT NOT NULL UNIQUE,
    email TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
