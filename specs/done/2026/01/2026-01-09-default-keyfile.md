# Default Key File

## Overview

When neither `PGL_KEY` nor `PGL_KEYFILE` environment variables are defined, PgLens should automatically use a default key file location and create it if it doesn't exist.

## Default Key File Location

The default key file path is: `~/.pglens/key`

## Behavior

### On Startup

1. Check if `PGL_KEY` is set - if yes, use it (existing behavior)
2. Check if `PGL_KEYFILE` is set - if yes, use it (existing behavior)
3. If neither is set:
   - Attempt to read the key from `~/.pglens/key`
   - If the file exists and contains a valid base64-encoded 32-byte key, use it
   - If the file doesn't exist, create it with a newly generated key

### Key Generation

When creating a new key file:

1. Create the `~/.pglens` directory if it doesn't exist (with mode 0700)
2. Generate a cryptographically secure random 32-byte key
3. Encode the key as base64
4. Write the base64-encoded key to `~/.pglens/key` (with mode 0600)
5. Log a message indicating a new key was generated

## File Format

The key file contains a single line with the base64-encoded 32-byte AES-256 key:

```
<base64-encoded-32-byte-key>
```

Example:
```
K7gNU3sdo+OL0wNhqoVWhr3g6s1xYv72ol/pe/Unols=
```

## Security Considerations

- The `~/.pglens` directory must have mode 0700 (owner read/write/execute only)
- The key file must have mode 0600 (owner read/write only)
- On key generation, log a warning that a new encryption key was created
- Document that losing this key means encrypted credentials cannot be recovered

## Error Handling

- If the directory cannot be created, fail with a clear error message
- If the file cannot be written, fail with a clear error message
- If the file exists but contains invalid content, fail with a clear error message
- If file permissions cannot be set correctly, fail with a clear error message

## Implementation Notes

- Use `os.UserHomeDir()` to get the home directory
- Use `crypto/rand` for key generation
- Use `encoding/base64.StdEncoding` for encoding
- Trim whitespace when reading the key file (to handle trailing newlines)
