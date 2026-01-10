# OpenAPI 3.0 Specification for DBBat REST API

## Summary

Create a comprehensive OpenAPI 3.0 specification file (`docs/openapi.yml`) that documents all REST API endpoints, request/response schemas, authentication mechanisms, and error responses.

## Motivation

The CLAUDE.md mentions that API documentation should be provided via OpenAPI 3.0 specification:
- Specification file: `docs/openapi.yml`
- Served at: `GET /api/openapi.yml`
- Interactive docs: `GET /api/docs` (Swagger UI)

Currently, no OpenAPI specification exists. Creating one will:
1. Provide machine-readable API documentation
2. Enable Swagger UI for interactive API exploration
3. Allow client SDK generation
4. Serve as a contract for API consumers

## Specification Details

### OpenAPI Version

Use OpenAPI 3.0.3 for broad tooling compatibility.

### Base Configuration

```yaml
openapi: 3.0.3
info:
  title: DBBat API
  description: REST API for DBBat PostgreSQL Observability Proxy
  version: 1.0.0
  contact:
    name: DBBat
servers:
  - url: /api
    description: API server
```

### Authentication Schemes

Define two security schemes:

```yaml
components:
  securitySchemes:
    basicAuth:
      type: http
      scheme: basic
      description: HTTP Basic Authentication (username:password)
    bearerAuth:
      type: http
      scheme: bearer
      description: API Key authentication (Bearer token)
```

### Endpoints to Document

#### Health Check
| Method | Path | Auth | Description |
|--------|------|------|-------------|
| GET | /health | None | Health check endpoint |

#### Users
| Method | Path | Auth | Description |
|--------|------|------|-------------|
| POST | /users | Admin | Create a new user |
| GET | /users | Admin | List all users |
| GET | /users/{uid} | Auth | Get user by UID |
| PUT | /users/{uid} | Auth | Update user (admin or self for password) |
| DELETE | /users/{uid} | Admin | Delete user |

#### Databases
| Method | Path | Auth | Description |
|--------|------|------|-------------|
| POST | /databases | Admin | Create database configuration |
| GET | /databases | Auth | List databases (role-filtered) |
| GET | /databases/{uid} | Auth | Get database by UID (role-filtered) |
| PUT | /databases/{uid} | Admin | Update database configuration |
| DELETE | /databases/{uid} | Admin | Delete database configuration |

#### Grants
| Method | Path | Auth | Description |
|--------|------|------|-------------|
| POST | /grants | Admin | Create access grant |
| GET | /grants | Auth | List grants (with filters) |
| GET | /grants/{uid} | Auth | Get grant by UID |
| DELETE | /grants/{uid} | Admin | Revoke grant |

#### API Keys
| Method | Path | Auth | Description |
|--------|------|------|-------------|
| POST | /keys | Basic Auth only | Create API key |
| GET | /keys | Auth | List API keys |
| GET | /keys/{id} | Auth | Get API key by ID |
| DELETE | /keys/{id} | Basic Auth only | Revoke API key |

#### Observability
| Method | Path | Auth | Description |
|--------|------|------|-------------|
| GET | /connections | Auth | List connections |
| GET | /queries | Admin/Viewer | List queries |
| GET | /queries/{uid} | Admin/Viewer | Get query with result rows |
| GET | /audit | Admin/Viewer | List audit events |

### Schema Definitions

#### User Schemas

```yaml
components:
  schemas:
    User:
      type: object
      properties:
        uid:
          type: string
          format: uuid
        username:
          type: string
        roles:
          type: array
          items:
            type: string
            enum: [admin, viewer, connector]
        rate_limit_exempt:
          type: boolean
        created_at:
          type: string
          format: date-time
        updated_at:
          type: string
          format: date-time

    CreateUserRequest:
      type: object
      required:
        - username
        - password
      properties:
        username:
          type: string
          minLength: 1
        password:
          type: string
          minLength: 1
        roles:
          type: array
          items:
            type: string
            enum: [admin, viewer, connector]

    UpdateUserRequest:
      type: object
      properties:
        password:
          type: string
        roles:
          type: array
          items:
            type: string
            enum: [admin, viewer, connector]
```

#### Database Schemas

```yaml
    Database:
      type: object
      properties:
        uid:
          type: string
          format: uuid
        name:
          type: string
        description:
          type: string
        host:
          type: string
        port:
          type: integer
        database_name:
          type: string
        username:
          type: string
        ssl_mode:
          type: string
        created_by:
          type: string
          format: uuid
        created_at:
          type: string
          format: date-time
        updated_at:
          type: string
          format: date-time

    DatabaseLimited:
      type: object
      description: Limited database info for non-admin users
      properties:
        uid:
          type: string
          format: uuid
        name:
          type: string
        description:
          type: string

    CreateDatabaseRequest:
      type: object
      required:
        - name
        - host
        - database_name
        - username
        - password
      properties:
        name:
          type: string
        description:
          type: string
        host:
          type: string
        port:
          type: integer
          default: 5432
        database_name:
          type: string
        username:
          type: string
        password:
          type: string
        ssl_mode:
          type: string
          default: prefer

    UpdateDatabaseRequest:
      type: object
      properties:
        description:
          type: string
        host:
          type: string
        port:
          type: integer
        database_name:
          type: string
        username:
          type: string
        password:
          type: string
        ssl_mode:
          type: string
```

#### Grant Schemas

```yaml
    AccessGrant:
      type: object
      properties:
        uid:
          type: string
          format: uuid
        user_id:
          type: string
          format: uuid
        database_id:
          type: string
          format: uuid
        access_level:
          type: string
          enum: [read, write]
        granted_by:
          type: string
          format: uuid
        starts_at:
          type: string
          format: date-time
        expires_at:
          type: string
          format: date-time
        revoked_at:
          type: string
          format: date-time
          nullable: true
        revoked_by:
          type: string
          format: uuid
          nullable: true
        max_query_counts:
          type: integer
          format: int64
          nullable: true
        max_bytes_transferred:
          type: integer
          format: int64
          nullable: true
        query_count:
          type: integer
          format: int64
        bytes_transferred:
          type: integer
          format: int64
        created_at:
          type: string
          format: date-time

    CreateGrantRequest:
      type: object
      required:
        - user_id
        - database_id
        - access_level
        - starts_at
        - expires_at
      properties:
        user_id:
          type: string
          format: uuid
        database_id:
          type: string
          format: uuid
        access_level:
          type: string
          enum: [read, write]
        starts_at:
          type: string
          format: date-time
        expires_at:
          type: string
          format: date-time
        max_query_counts:
          type: integer
          format: int64
        max_bytes_transferred:
          type: integer
          format: int64
```

#### API Key Schemas

```yaml
    APIKey:
      type: object
      properties:
        id:
          type: string
          format: uuid
        user_id:
          type: string
          format: uuid
        name:
          type: string
        key_prefix:
          type: string
        expires_at:
          type: string
          format: date-time
          nullable: true
        last_used_at:
          type: string
          format: date-time
          nullable: true
        request_count:
          type: integer
          format: int64
        created_at:
          type: string
          format: date-time
        revoked_at:
          type: string
          format: date-time
          nullable: true
        revoked_by:
          type: string
          format: uuid
          nullable: true

    CreateAPIKeyRequest:
      type: object
      required:
        - name
      properties:
        name:
          type: string
        expires_at:
          type: string
          format: date-time

    CreateAPIKeyResponse:
      type: object
      properties:
        id:
          type: string
          format: uuid
        name:
          type: string
        key:
          type: string
          description: Full API key (only shown once at creation)
        key_prefix:
          type: string
        expires_at:
          type: string
          format: date-time
          nullable: true
        created_at:
          type: string
          format: date-time
```

#### Connection Schemas

```yaml
    Connection:
      type: object
      properties:
        uid:
          type: string
          format: uuid
        user_id:
          type: string
          format: uuid
        database_id:
          type: string
          format: uuid
        source_ip:
          type: string
        connected_at:
          type: string
          format: date-time
        last_activity_at:
          type: string
          format: date-time
        disconnected_at:
          type: string
          format: date-time
          nullable: true
        queries:
          type: integer
          format: int64
        bytes_transferred:
          type: integer
          format: int64
```

#### Query Schemas

```yaml
    Query:
      type: object
      properties:
        uid:
          type: string
          format: uuid
        connection_id:
          type: string
          format: uuid
        sql_text:
          type: string
        parameters:
          $ref: '#/components/schemas/QueryParameters'
        executed_at:
          type: string
          format: date-time
        duration_ms:
          type: number
          format: double
        rows_affected:
          type: integer
          format: int64
        error:
          type: string
          nullable: true

    QueryParameters:
      type: object
      properties:
        values:
          type: array
          items:
            type: string
        raw:
          type: array
          items:
            type: string
            description: Base64-encoded raw bytes
        format_codes:
          type: array
          items:
            type: integer
        type_oids:
          type: array
          items:
            type: integer

    QueryWithRows:
      allOf:
        - $ref: '#/components/schemas/Query'
        - type: object
          properties:
            rows:
              type: array
              items:
                $ref: '#/components/schemas/QueryResultRow'

    QueryResultRow:
      type: object
      properties:
        row_number:
          type: integer
        row_data:
          type: object
          additionalProperties: true
        row_size_bytes:
          type: integer
          format: int64
```

#### Audit Schemas

```yaml
    AuditEvent:
      type: object
      properties:
        uid:
          type: string
          format: uuid
        event_type:
          type: string
          description: Event type (e.g., user.created, grant.revoked)
        user_id:
          type: string
          format: uuid
          nullable: true
        performed_by:
          type: string
          format: uuid
        details:
          type: object
          additionalProperties: true
        created_at:
          type: string
          format: date-time
```

#### Error and Common Schemas

```yaml
    Error:
      type: object
      properties:
        error:
          type: string

    RateLimitError:
      type: object
      properties:
        error:
          type: string
          example: rate_limit_exceeded
        message:
          type: string
        retry_after:
          type: integer

    MessageResponse:
      type: object
      properties:
        message:
          type: string
```

### Rate Limiting Headers

Document rate limit headers on all responses:

```yaml
components:
  headers:
    X-RateLimit-Limit:
      description: Maximum requests per minute
      schema:
        type: integer
    X-RateLimit-Remaining:
      description: Remaining requests in current window
      schema:
        type: integer
    X-RateLimit-Reset:
      description: Unix timestamp when limit resets
      schema:
        type: integer
    Retry-After:
      description: Seconds until retry is allowed (on 429)
      schema:
        type: integer
```

### Common Responses

```yaml
components:
  responses:
    Unauthorized:
      description: Authentication required
      content:
        application/json:
          schema:
            $ref: '#/components/schemas/Error'
    Forbidden:
      description: Insufficient permissions
      content:
        application/json:
          schema:
            $ref: '#/components/schemas/Error'
    NotFound:
      description: Resource not found
      content:
        application/json:
          schema:
            $ref: '#/components/schemas/Error'
    BadRequest:
      description: Invalid request
      content:
        application/json:
          schema:
            $ref: '#/components/schemas/Error'
    RateLimited:
      description: Rate limit exceeded
      headers:
        Retry-After:
          $ref: '#/components/headers/Retry-After'
      content:
        application/json:
          schema:
            $ref: '#/components/schemas/RateLimitError'
```

### Tags

Organize endpoints by resource:

```yaml
tags:
  - name: Health
    description: Health check endpoints
  - name: Users
    description: User management
  - name: Databases
    description: Database configuration management
  - name: Grants
    description: Access grant management
  - name: API Keys
    description: API key management
  - name: Connections
    description: Connection observability
  - name: Queries
    description: Query observability
  - name: Audit
    description: Audit log
```

## Implementation

### File Location

Create the specification at: `docs/openapi.yml`

### Serving the Specification

The API server should:
1. Embed the `docs/openapi.yml` file at compile time
2. Serve it at `GET /api/openapi.yml`
3. Serve Swagger UI at `GET /api/docs` using `swaggo/gin-swagger` or similar

### Example Endpoint Documentation

```yaml
paths:
  /users:
    post:
      tags:
        - Users
      summary: Create a new user
      description: Creates a new user account. Requires admin role.
      operationId: createUser
      security:
        - basicAuth: []
        - bearerAuth: []
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/CreateUserRequest'
      responses:
        '201':
          description: User created successfully
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/User'
        '400':
          $ref: '#/components/responses/BadRequest'
        '401':
          $ref: '#/components/responses/Unauthorized'
        '403':
          $ref: '#/components/responses/Forbidden'
        '429':
          $ref: '#/components/responses/RateLimited'

    get:
      tags:
        - Users
      summary: List all users
      description: Returns a list of all users. Requires admin role.
      operationId: listUsers
      security:
        - basicAuth: []
        - bearerAuth: []
      responses:
        '200':
          description: List of users
          content:
            application/json:
              schema:
                type: object
                properties:
                  users:
                    type: array
                    items:
                      $ref: '#/components/schemas/User'
        '401':
          $ref: '#/components/responses/Unauthorized'
        '403':
          $ref: '#/components/responses/Forbidden'
        '429':
          $ref: '#/components/responses/RateLimited'
```

## Acceptance Criteria

1. **Complete Specification**: All endpoints documented with request/response schemas
2. **Authentication**: Both Basic Auth and Bearer token schemes documented
3. **Error Responses**: All error types (400, 401, 403, 404, 429, 500) documented
4. **Rate Limiting**: Headers and 429 response documented
5. **Examples**: Request/response examples for key endpoints
6. **Validation**: Specification passes OpenAPI validation (e.g., using `swagger-cli validate`)
7. **Serving**: Specification served at `/api/openapi.yml`
8. **Swagger UI**: Interactive documentation available at `/api/docs`

## Out of Scope

- Generating client SDKs (can be done with the spec but not part of this task)
- Code generation from the spec (the spec documents existing implementation)
- API versioning (can be added in a future iteration)
