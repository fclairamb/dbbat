-- Create DBBat database
CREATE DATABASE dbbat;

-- Create target database for testing
CREATE DATABASE target;

-- Create demo database and user
CREATE DATABASE demo;
CREATE USER demo WITH PASSWORD 'demo';
GRANT ALL PRIVILEGES ON DATABASE demo TO demo;

-- Connect to target database and create test table
\c target

CREATE TABLE test_data (
    id SERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    value INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Insert some test data
INSERT INTO test_data (name, value) VALUES
    ('Test 1', 100),
    ('Test 2', 200),
    ('Test 3', 300);

\c demo

-- Grant schema permissions to demo user
GRANT ALL ON SCHEMA public TO demo;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO demo;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO demo;

CREATE TABLE demo_data (
    id SERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    value INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Grant permissions on existing tables
GRANT ALL ON ALL TABLES IN SCHEMA public TO demo;
GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO demo;

-- Insert some test data
INSERT INTO demo_data (name, value) VALUES
    ('Demo 1', 100),
    ('Demo 2', 200),
    ('Demo 3', 300);

\c
