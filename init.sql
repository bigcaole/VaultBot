-- VaultBot initialization
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS accounts (
    id uuid PRIMARY KEY,
    platform varchar(128),
    category varchar(128),
    username varchar(128),
    encrypted_password text,
    email varchar(256),
    phone varchar(64),
    notes text,
    nonce text,
    created_at timestamptz,
    updated_at timestamptz
);

CREATE INDEX IF NOT EXISTS idx_accounts_platform ON accounts (platform);
CREATE INDEX IF NOT EXISTS idx_accounts_category ON accounts (category);
CREATE INDEX IF NOT EXISTS idx_accounts_username ON accounts (username);
