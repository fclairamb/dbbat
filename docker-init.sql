-- Create DBBat database
CREATE DATABASE dbbat;

-- Create target database for testing
CREATE DATABASE target;

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
