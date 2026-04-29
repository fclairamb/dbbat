---
sidebar_position: 2
---

# Docker Compose

For development and testing, Docker Compose provides an easy way to run DBBat with all dependencies.

## docker-compose.yml

```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: dbbat
      POSTGRES_PASSWORD: dbbat
      POSTGRES_DB: dbbat
    ports:
      - "5000:5432"
    volumes:
      - postgres_data:/var/lib/postgresql/data
      - ./init.sql:/docker-entrypoint-initdb.d/init.sql:ro

  dbbat:
    image: ghcr.io/fclairamb/dbbat
    depends_on:
      - postgres
    environment:
      DBB_DSN: postgres://dbbat:dbbat@postgres:5432/dbbat?sslmode=disable
      DBB_KEY: ${DBB_KEY:-YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY=}
      DBB_LISTEN_PG: ":5434"
      DBB_LISTEN_ORA: ":1522"
      DBB_LISTEN_MYSQL: ":3307"
      DBB_LISTEN_API: ":4200"
    ports:
      - "5001:5434"   # PostgreSQL proxy
      - "1522:1522"   # Oracle proxy
      - "3307:3307"   # MySQL / MariaDB proxy
      - "4200:4200"   # REST API + web UI

volumes:
  postgres_data:
```

Drop any `DBB_LISTEN_*`/port pair you don't need — set the variable to an empty string to disable that proxy entirely.

## Initial Setup Script (init.sql)

Create an `init.sql` file to set up a sample target database:

```sql
-- Create a target database for testing
CREATE DATABASE target;

\c target

CREATE TABLE test_data (
    id SERIAL PRIMARY KEY,
    name VARCHAR(100),
    value INTEGER,
    created_at TIMESTAMP DEFAULT NOW()
);

INSERT INTO test_data (name, value) VALUES
    ('Test 1', 100),
    ('Test 2', 200),
    ('Test 3', 300);
```

## Usage

Start the services:

```bash
docker compose up -d
```

### Login and Get a Token

```bash
TOKEN=$(curl -s -X POST http://localhost:4200/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username": "admin", "password": "admin"}' | jq -r '.token')
```

:::note
On first start the default admin password is flagged as requiring change. Call `PUT /api/v1/auth/password` to set a real password before logging in:

```bash
curl -X PUT http://localhost:4200/api/v1/auth/password \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","current_password":"admin","new_password":"NewSecurePass!"}'
```
:::

### Configure a Target Database

```bash
# PostgreSQL target
curl -X POST http://localhost:4200/api/v1/databases \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "target_db",
    "description": "Target database for testing",
    "protocol": "postgresql",
    "host": "postgres",
    "port": 5432,
    "database_name": "target",
    "username": "dbbat",
    "password": "dbbat",
    "ssl_mode": "disable"
  }'
```

For Oracle add `"protocol": "oracle"` plus `"oracle_service_name": "ORCL"`. For MySQL/MariaDB use `"protocol": "mysql"` (or `"mariadb"`) and port `3306`.

### Create a Test User and Grant Access

```bash
# Create the user
USER_UID=$(curl -s -X POST http://localhost:4200/api/v1/users \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"username": "testuser", "password": "testpass", "roles": ["connector"]}' \
  | jq -r '.uid')

# Activate the user (one-time pre-login password set)
curl -X PUT http://localhost:4200/api/v1/auth/password \
  -H "Content-Type: application/json" \
  -d '{"username":"testuser","current_password":"testpass","new_password":"newtestpass"}'

# Get the database UID
DB_UID=$(curl -s -H "Authorization: Bearer $TOKEN" \
  http://localhost:4200/api/v1/databases | jq -r '.databases[0].uid')

# Create a grant (empty controls = full write access)
curl -X POST http://localhost:4200/api/v1/grants \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"user_id\": \"$USER_UID\",
    \"database_id\": \"$DB_UID\",
    \"controls\": [],
    \"starts_at\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",
    \"expires_at\": \"2030-01-01T00:00:00Z\"
  }"
```

:::note
An empty `controls` array means full write access. Use `["read_only"]` for read-only access, or combine `["read_only", "block_copy", "block_ddl"]`.
:::

### Connect Through the Proxy

```bash
# PostgreSQL
PGPASSWORD=newtestpass psql -h localhost -p 5001 -U testuser -d target_db

# MySQL / MariaDB (against a MySQL target)
mysql -h 127.0.0.1 -P 3307 -u testuser -p target_db

# Oracle (against an Oracle target — using go-ora-style easy connect)
# user/pass@//localhost:1522/target_db
```

## Stopping

```bash
docker compose down
```

To also remove volumes:

```bash
docker compose down -v
```

## Next Steps

- [Configure DBBat](/docs/configuration)
- [Kubernetes deployment](/docs/installation/kubernetes)
