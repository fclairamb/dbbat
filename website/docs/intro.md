---
sidebar_position: 1
---

# Introduction to DBBat

DBBat is a transparent PostgreSQL proxy designed for query observability, access control, and safety. 

It allows to give temporary access to production databases for support, debugging and/or data analysis.

It can be used with a standard SQL client like psql, DBeave, pgAdmin, or even the standard app 
(though it's not recommended in production).

## Why DBBat?

Giving access to production databases can be dangerous. DBBat provides:
- **Query visibility**: Know what queries are being executed, how long they take, and what data they return
- **Access control**: Grant temporary, limited access to databases for support, debugging, or data analysis
- **Audit trails**: Maintain a complete record of who accessed what data and when
- **Safety**: Prevent accidental writes to production databases

DBBat addresses all these needs without requiring changes to your application code.

## Core Features

### Transparent Proxy

DBBat speaks the native PostgreSQL wire protocol. Any PostgreSQL client (psql, pgAdmin, your application's ORM) can connect through DBBat without modification.

```
Client → DBBat (auth + grant check) → Target PostgreSQL
```

### User Management

- Users authenticate to DBBat with their own credentials
- Admin users can create/modify other users and manage all resources
- User credentials are separate from target database credentials

### Database Configuration

- Store multiple target database connection details
- Credentials encrypted at rest with AES-256-GCM
- Map DBBat database names to target PostgreSQL servers

### Connection & Query Tracking

- Track all connections with user, timestamp, source IP, and target database
- Log all queries with SQL text, execution time, and rows affected
- Optionally store query result data for audit/replay

### Access Control

- Grant time-windowed access (starts_at, expires_at) to specific databases
- Access levels: `read` or `write`
- Optional quotas: max queries, max bytes transferred
- Automatic expiration or manual revocation
- Full audit log of all access control changes

## How It Works

Everything described here can be done via the REST API or the web UI.

1. **Admin creates a user**
2. **Admin configures a target database** (host, port, credentials)
3. **Admin grants the user access** to the database with time window and optional quotas
4. **User connects** with psql or any PostgreSQL client using their DBBat credentials
5. **DBBat authenticates** the user and checks for valid grants
6. **DBBat proxies** all queries to the target database, logging everything

## Security

- **User passwords**: Hashed with Argon2id
- **Database credentials**: Encrypted with AES-256-GCM
- **Encryption key**: From environment variable or key file
- **Default admin**: Created on first startup (username: `admin`, password: `admin`)

## Try the Demo

Experience DBBat without any setup. Our demo instance is available at:

**[demo.dbbat.com](https://demo.dbbat.com)**

- Login: `admin` / `admin`
- Data resets periodically
- Explore all features freely

## Next Steps

- [Install DBBat](/docs/installation/docker) using Docker
- [Configure](/docs/configuration) your environment
- Learn about [Access Control](/docs/features/access-control)
