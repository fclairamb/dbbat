-- DBBat Database Schema - Initial Migration

-- User roles enum
CREATE TYPE user_role AS ENUM ('admin', 'viewer', 'connector');

-- DBBat users (for authentication)
CREATE TABLE users (
    uid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    roles user_role[] NOT NULL DEFAULT ARRAY['connector']::user_role[],
    rate_limit_exempt BOOLEAN NOT NULL DEFAULT FALSE,
    password_changed_at TIMESTAMPTZ,              -- NULL means initial password not yet changed
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ                        -- Soft delete timestamp
);
CREATE INDEX idx_users_username ON users(username);
CREATE INDEX idx_users_roles ON users USING GIN(roles);
CREATE INDEX idx_users_deleted_at ON users(deleted_at) WHERE deleted_at IS NULL;

--bun:split

-- API Keys for programmatic access
-- key_type: 'api' for regular API keys (dbb_ prefix), 'web' for web sessions (web_ prefix)
CREATE TABLE api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    key_hash VARCHAR(255) NOT NULL,
    key_prefix VARCHAR(8) NOT NULL,
    key_type VARCHAR(10) NOT NULL DEFAULT 'api',
    expires_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    request_count BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ,
    revoked_by UUID REFERENCES users(uid),

    CONSTRAINT unique_key_prefix UNIQUE (key_prefix),
    CONSTRAINT chk_key_type CHECK (key_type IN ('api', 'web'))
);
CREATE INDEX idx_api_keys_user_id ON api_keys(user_id);
CREATE INDEX idx_api_keys_key_prefix ON api_keys(key_prefix);
CREATE INDEX idx_api_keys_key_type ON api_keys(key_type);

--bun:split

-- Target database configurations
CREATE TABLE databases (
    uid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE,              -- Name used in PgLens connection string
    description TEXT,
    host TEXT NOT NULL,
    port INT NOT NULL DEFAULT 5432,
    database_name TEXT NOT NULL,            -- Actual database name on target
    username TEXT NOT NULL,                 -- Username for target connection
    password_encrypted BYTEA NOT NULL,      -- AES-256-GCM encrypted password
    ssl_mode TEXT NOT NULL DEFAULT 'prefer',
    created_by UUID REFERENCES users(uid),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_databases_name ON databases(name);

--bun:split

-- Track all connections through the proxy
CREATE TABLE connections (
    uid UUID PRIMARY KEY,                   -- UUIDv7 generated in Go
    user_id UUID NOT NULL REFERENCES users(uid),
    database_id UUID NOT NULL REFERENCES databases(uid),
    source_ip INET NOT NULL,
    connected_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_activity_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    disconnected_at TIMESTAMPTZ,
    queries INT NOT NULL DEFAULT 0,
    bytes_transferred BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX idx_connections_user_id ON connections(user_id);
CREATE INDEX idx_connections_database_id ON connections(database_id);
CREATE INDEX idx_connections_connected_at ON connections(connected_at);

--bun:split

-- Track all queries executed
CREATE TABLE queries (
    uid UUID PRIMARY KEY,                   -- UUIDv7 generated in Go
    connection_id UUID NOT NULL REFERENCES connections(uid) ON DELETE CASCADE,
    sql_text TEXT NOT NULL,
    parameters jsonb,
    executed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    duration_ms NUMERIC(10,3),
    rows_affected BIGINT,
    error TEXT,
    copy_format TEXT,                       -- 'text', 'csv', 'binary', or NULL for non-COPY queries
    copy_direction TEXT                     -- 'in' (COPY FROM), 'out' (COPY TO), or NULL
);
CREATE INDEX idx_queries_connection_id ON queries(connection_id);
CREATE INDEX idx_queries_executed_at ON queries(executed_at);

--bun:split

-- Store query result/COPY data rows (with retention limits)
CREATE TABLE query_rows (
    uid UUID PRIMARY KEY,                   -- UUIDv7 generated in Go
    query_id UUID NOT NULL REFERENCES queries(uid) ON DELETE CASCADE,
    row_number INT NOT NULL,
    row_data JSONB NOT NULL,
    row_size_bytes BIGINT NOT NULL
);
CREATE INDEX idx_query_rows_query_id ON query_rows(query_id);

--bun:split

-- Manage access grants
CREATE TABLE access_grants (
    uid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(uid),
    database_id UUID NOT NULL REFERENCES databases(uid),
    access_level TEXT NOT NULL CHECK (access_level IN ('read', 'write')),
    granted_by UUID NOT NULL REFERENCES users(uid),
    starts_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    revoked_by UUID REFERENCES users(uid),
    max_query_counts INT,                   -- NULL = unlimited
    max_bytes_transferred BIGINT,           -- NULL = unlimited
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT valid_time_window CHECK (starts_at < expires_at)
);
CREATE INDEX idx_access_grants_user_id ON access_grants(user_id);
CREATE INDEX idx_access_grants_database_id ON access_grants(database_id);
CREATE INDEX idx_access_grants_expires_at ON access_grants(expires_at);
-- Note: Can't use NOW() in partial index as it's not IMMUTABLE
-- Active grant uniqueness is enforced at application level
CREATE INDEX idx_access_grants_active ON access_grants(user_id, database_id)
    WHERE revoked_at IS NULL;

--bun:split

-- Audit log for access control changes
CREATE TABLE audit_log (
    uid UUID PRIMARY KEY,                   -- UUIDv7 generated in Go
    event_type TEXT NOT NULL,
    user_id UUID REFERENCES users(uid),     -- User affected
    performed_by UUID REFERENCES users(uid),-- Admin who performed action
    details JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_audit_log_user_id ON audit_log(user_id);
CREATE INDEX idx_audit_log_performed_by ON audit_log(performed_by);
CREATE INDEX idx_audit_log_event_type ON audit_log(event_type);
CREATE INDEX idx_audit_log_created_at ON audit_log(created_at);
