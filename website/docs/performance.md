# Performance

DBBat prioritizes **observability and security over performance**. This is by design—the primary use case is controlled developer access to production data, not high-throughput application workloads.

## Overhead Sources

### Latency Per Query

Each query incurs additional latency from:

| Source | Typical Impact |
|--------|----------------|
| Extra network hop | 0.1-1ms (same host) to 1-10ms (cross-network) |
| Query inspection | < 0.1ms (regex-based checks) |
| Grant/quota validation | < 0.1ms (in-memory after first lookup) |
| Query logging (async) | Negligible (non-blocking) |
| Result row capture | Proportional to result size |
| SSH tunnel hop (when `via_uid` is set) | One extra network hop to the bastion, plus SSH encryption/decryption of the forwarded stream |

**Estimated total overhead**: 1-5ms per query in typical deployments, plus time proportional to result set size.

### Result Capture

DBBat captures and stores all query results for audit purposes:

- Results are buffered in memory before forwarding to the client
- Each row is serialized and written to the database asynchronously
- Large result sets (thousands of rows, megabytes of data) will have noticeable overhead

### Connection Handling

DBBat maintains a 1:1 mapping between client connections and upstream **database** connections, for every engine (PostgreSQL, Oracle, MySQL/MariaDB, MongoDB):

- Each client connection opens a dedicated upstream connection
- No database connection reuse or pooling between different client sessions
- Connection establishment adds latency on first connect

**SSH transport connections are the exception.** When a server is reached through an SSH bastion (`via_uid`), the SSH transport itself is pooled: a single SSH connection to a given bastion is shared by many proxied sessions, and each session opens its own forwarded channel over it. The database connections carried inside remain 1:1 — only the SSH transport underneath is reused. The first session to a bastion pays the SSH handshake; later ones do not.

## What's NOT Affected

- **Query execution time**: the upstream engine processes queries identically
- **Query planning**: no impact on the upstream query optimizer
- **Index usage**: the engine uses indexes the same way
- **Transaction semantics**: full ACID compliance preserved

## Appropriate Use Cases

DBBat is designed for:

- **Ad-hoc debugging queries**: Developers troubleshooting production issues
- **Data exploration**: Understanding data patterns and relationships
- **Audit compliance**: Environments requiring query logging and access control
- **Time-limited access**: Temporary grants for specific investigations
- **Low-frequency queries**: Occasional queries, not sustained throughput

## Inappropriate Use Cases

Do **not** use DBBat for:

- **Production application traffic**: Use direct connections or PgBouncer
- **High-throughput workloads**: Batch processing, ETL pipelines
- **Low-latency requirements**: Real-time applications needing sub-millisecond response
- **Bulk data operations**: Large imports, exports, or COPY operations
- **Connection-pooled applications**: Web apps expecting connection reuse

## Recommendations

### For Developers

- Use DBBat only when you need the observability features
- For routine development, use direct database connections
- Expect queries to take slightly longer than direct access

### For Operators

- Deploy DBBat close to the target database (same network/region)
- Monitor DBBat's storage database for growth from query logs
- Set appropriate quotas to prevent runaway queries
- Consider result row retention policies for large deployments

### Query Patterns to Avoid

- `SELECT *` on large tables without `LIMIT`
- Queries returning millions of rows
- Long-running transactions holding connections
- Rapid-fire queries in tight loops

## Benchmarks

No formal benchmarks are published. Performance varies significantly based on:

- Network topology between client, DBBat, and PostgreSQL
- Query complexity and result set sizes
- DBBat storage database performance
- Concurrent connection count

For your specific deployment, test representative queries with and without DBBat to measure actual overhead.
