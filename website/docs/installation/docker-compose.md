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
      DBB_LISTEN_PG: ":5432"
      DBB_LISTEN_API: ":8080"
    ports:
      - "5001:5432"  # Proxy port
      - "8080:8080"  # API port

volumes:
  postgres_data:
```

## Initial Setup Script (init.sql)

Create a `init.sql` file to set up a sample target database:

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
docker-compose up -d
```

Create a test user and grant access:

```bash
# Create a test user
curl -u admin:admin -X POST http://localhost:8080/api/users \
  -H "Content-Type: application/json" \
  -d '{"username": "testuser", "password": "testpass", "is_admin": false}'

# Create a grant (adjust user_id and database_id as needed)
curl -u admin:admin -X POST http://localhost:8080/api/grants \
  -H "Content-Type: application/json" \
  -d '{
    "user_id": 2,
    "database_id": 1,
    "access_level": "write",
    "starts_at": "2024-01-01T00:00:00Z",
    "expires_at": "2030-01-01T00:00:00Z"
  }'
```

Connect through the proxy:

```bash
PGPASSWORD=testpass psql -h localhost -p 5001 -U testuser -d proxy_target
```

## Stopping

```bash
docker-compose down
```

To also remove volumes:

```bash
docker-compose down -v
```

## Next Steps

- [Configure DBBat](/docs/configuration)
- [Kubernetes deployment](/docs/installation/kubernetes)
