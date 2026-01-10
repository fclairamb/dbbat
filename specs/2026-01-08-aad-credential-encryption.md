# Spec: AAD-Bound Credential Encryption

**Date**: 2026-01-08
**Status**: Draft
**Author**: Claude

## Summary

PgLens encrypts database credentials using AES-256-GCM before storing them in the `databases` table. While the current implementation correctly uses random nonces, it does not bind the ciphertext to its context (the database row). This means an attacker with database access could swap encrypted credentials between rows without detection.

This specification adds Additional Authenticated Data (AAD) to bind encrypted credentials to their database ID, preventing credential transplant attacks.

## Problem Statement

### Current Implementation

The encryption in `internal/crypto/encrypt.go` uses AES-256-GCM correctly:

```go
gcm.Seal(nonce, nonce, plaintext, nil)  // nil AAD
```

However, passing `nil` for AAD means the ciphertext is not bound to any context.

### Attack Scenario

1. Attacker gains read/write access to the `databases` table (SQL injection, backup theft, etc.)
2. Attacker swaps `password_encrypted` between two database configurations
3. When PgLens connects to "production" DB, it uses credentials from "staging" DB
4. This could expose staging credentials to production, or vice versa

### Why AAD Prevents This

With AAD binding:
- Credential for database ID 1 is encrypted with `AAD = "database:1"`
- Credential for database ID 2 is encrypted with `AAD = "database:2"`
- If attacker swaps ciphertexts, decryption fails because AAD doesn't match
- GCM authentication tag verification detects the mismatch

## Design

### Goals

1. **Context binding**: Encrypted credentials can only be decrypted for their original database
2. **Backward compatibility**: Graceful handling of existing data without AAD
3. **Minimal API change**: Add optional AAD parameter to existing functions
4. **Defense in depth**: Additional security layer beyond encryption key

### AAD Format

Use a simple, predictable format:

```
database:<id>
```

Example: `database:42`

This format is:
- Deterministic (can be reconstructed from database ID)
- Human-readable for debugging
- Unambiguous (no confusion with other encrypted fields)

### API Changes

Update `Encrypt` and `Decrypt` to accept optional AAD:

```go
// Encrypt encrypts plaintext using AES-256-GCM with optional AAD binding.
func Encrypt(plaintext []byte, key []byte, aad []byte) ([]byte, error)

// Decrypt decrypts ciphertext using AES-256-GCM with optional AAD binding.
func Decrypt(ciphertext []byte, key []byte, aad []byte) ([]byte, error)
```

For backward compatibility, `nil` AAD continues to work for existing data.

## Implementation

### 1. Update Encrypt Function

**File**: `internal/crypto/encrypt.go`

```go
// Encrypt encrypts plaintext using AES-256-GCM with the provided key.
// The ciphertext includes the nonce prefix.
// Optional aad (Additional Authenticated Data) binds the ciphertext to a context.
func Encrypt(plaintext []byte, key []byte, aad []byte) ([]byte, error) {
    if len(key) != 32 {
        return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKeySize, len(key))
    }

    block, err := aes.NewCipher(key)
    if err != nil {
        return nil, fmt.Errorf("failed to create cipher: %w", err)
    }

    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return nil, fmt.Errorf("failed to create GCM: %w", err)
    }

    nonce := make([]byte, gcm.NonceSize())
    if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
        return nil, fmt.Errorf("failed to generate nonce: %w", err)
    }

    // Encrypt with AAD binding
    ciphertext := gcm.Seal(nonce, nonce, plaintext, aad)

    return ciphertext, nil
}
```

### 2. Update Decrypt Function

**File**: `internal/crypto/encrypt.go`

```go
// Decrypt decrypts ciphertext using AES-256-GCM with the provided key.
// The ciphertext must include the nonce prefix.
// The aad must match the value used during encryption, or be nil for legacy data.
func Decrypt(ciphertext []byte, key []byte, aad []byte) ([]byte, error) {
    if len(key) != 32 {
        return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKeySize, len(key))
    }

    block, err := aes.NewCipher(key)
    if err != nil {
        return nil, fmt.Errorf("failed to create cipher: %w", err)
    }

    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return nil, fmt.Errorf("failed to create GCM: %w", err)
    }

    nonceSize := gcm.NonceSize()
    if len(ciphertext) < nonceSize {
        return nil, ErrCiphertextTooShort
    }

    nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]

    // Decrypt with AAD verification
    plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
    if err != nil {
        return nil, fmt.Errorf("failed to decrypt: %w", err)
    }

    return plaintext, nil
}
```

### 3. Add Helper Function for Database AAD

**File**: `internal/crypto/encrypt.go`

```go
// DatabaseAAD returns the AAD for encrypting database credentials.
func DatabaseAAD(databaseID int64) []byte {
    return []byte(fmt.Sprintf("database:%d", databaseID))
}
```

### 4. Update Database Store

**File**: `internal/store/databases.go`

Update `CreateDatabase` to encrypt with AAD after getting the ID:

```go
func (s *Store) CreateDatabase(ctx context.Context, db *Database) (*Database, error) {
    // First insert with empty password to get the ID
    db.CreatedAt = time.Now()
    db.UpdatedAt = db.CreatedAt

    // Temporarily store password, encrypt after we have ID
    plainPassword := db.PasswordEncrypted
    db.PasswordEncrypted = nil

    _, err := s.db.NewInsert().Model(db).Exec(ctx)
    if err != nil {
        return nil, fmt.Errorf("failed to create database: %w", err)
    }

    // Now encrypt with AAD bound to database ID
    if len(plainPassword) > 0 {
        aad := crypto.DatabaseAAD(db.ID)
        encrypted, err := crypto.Encrypt(plainPassword, s.encryptionKey, aad)
        if err != nil {
            // Rollback: delete the database we just created
            _, _ = s.db.NewDelete().Model(db).WherePK().Exec(ctx)
            return nil, fmt.Errorf("failed to encrypt password: %w", err)
        }

        db.PasswordEncrypted = encrypted
        _, err = s.db.NewUpdate().Model(db).Column("password_encrypted").WherePK().Exec(ctx)
        if err != nil {
            return nil, fmt.Errorf("failed to update encrypted password: %w", err)
        }
    }

    return db, nil
}
```

Update `GetDatabaseByName` and `GetDatabaseByID` to decrypt with AAD:

```go
func (s *Store) GetDatabaseByName(ctx context.Context, name string) (*Database, error) {
    db := new(Database)
    err := s.db.NewSelect().Model(db).Where("name = ?", name).Scan(ctx)
    if err != nil {
        return nil, fmt.Errorf("failed to get database: %w", err)
    }

    // Decrypt password with AAD
    if len(db.PasswordEncrypted) > 0 {
        aad := crypto.DatabaseAAD(db.ID)
        decrypted, err := crypto.Decrypt(db.PasswordEncrypted, s.encryptionKey, aad)
        if err != nil {
            // Try without AAD for backward compatibility
            decrypted, err = crypto.Decrypt(db.PasswordEncrypted, s.encryptionKey, nil)
            if err != nil {
                return nil, fmt.Errorf("failed to decrypt password: %w", err)
            }
        }
        db.PasswordDecrypted = string(decrypted)
    }

    return db, nil
}
```

### 5. Migration for Existing Data (Optional)

For existing databases encrypted without AAD, a migration can re-encrypt with AAD:

```go
func (s *Store) MigrateToAADEncryption(ctx context.Context) error {
    var databases []Database
    err := s.db.NewSelect().Model(&databases).Scan(ctx)
    if err != nil {
        return err
    }

    for _, db := range databases {
        if len(db.PasswordEncrypted) == 0 {
            continue
        }

        // Try to decrypt without AAD (old format)
        plaintext, err := crypto.Decrypt(db.PasswordEncrypted, s.encryptionKey, nil)
        if err != nil {
            // Already migrated or corrupt
            continue
        }

        // Re-encrypt with AAD
        aad := crypto.DatabaseAAD(db.ID)
        newCiphertext, err := crypto.Encrypt(plaintext, s.encryptionKey, aad)
        if err != nil {
            return fmt.Errorf("failed to re-encrypt database %d: %w", db.ID, err)
        }

        _, err = s.db.NewUpdate().
            Model(&db).
            Set("password_encrypted = ?", newCiphertext).
            WherePK().
            Exec(ctx)
        if err != nil {
            return fmt.Errorf("failed to update database %d: %w", db.ID, err)
        }
    }

    return nil
}
```

## Testing

### Unit Tests

**File**: `internal/crypto/encrypt_test.go`

```go
func TestEncryptDecryptWithAAD(t *testing.T) {
    key := make([]byte, 32)
    rand.Read(key)
    plaintext := []byte("secret-password-123")
    aad := []byte("database:42")

    ciphertext, err := Encrypt(plaintext, key, aad)
    if err != nil {
        t.Fatalf("Encrypt: %v", err)
    }

    // Decrypt with correct AAD
    decrypted, err := Decrypt(ciphertext, key, aad)
    if err != nil {
        t.Fatalf("Decrypt with correct AAD: %v", err)
    }
    if string(decrypted) != string(plaintext) {
        t.Errorf("got %q, want %q", decrypted, plaintext)
    }
}

func TestDecryptWithWrongAADFails(t *testing.T) {
    key := make([]byte, 32)
    rand.Read(key)
    plaintext := []byte("secret-password-123")
    aad := []byte("database:42")
    wrongAAD := []byte("database:99")

    ciphertext, err := Encrypt(plaintext, key, aad)
    if err != nil {
        t.Fatalf("Encrypt: %v", err)
    }

    // Decrypt with wrong AAD should fail
    _, err = Decrypt(ciphertext, key, wrongAAD)
    if err == nil {
        t.Fatal("expected error when decrypting with wrong AAD")
    }
}

func TestDecryptWithNilAADWhenEncryptedWithAADFails(t *testing.T) {
    key := make([]byte, 32)
    rand.Read(key)
    plaintext := []byte("secret-password-123")
    aad := []byte("database:42")

    ciphertext, err := Encrypt(plaintext, key, aad)
    if err != nil {
        t.Fatalf("Encrypt: %v", err)
    }

    // Decrypt with nil AAD should fail
    _, err = Decrypt(ciphertext, key, nil)
    if err == nil {
        t.Fatal("expected error when decrypting with nil AAD")
    }
}

func TestDatabaseAAD(t *testing.T) {
    aad := DatabaseAAD(42)
    expected := []byte("database:42")
    if string(aad) != string(expected) {
        t.Errorf("got %q, want %q", aad, expected)
    }
}
```

## Files to Modify

| File | Changes |
|------|---------|
| `internal/crypto/encrypt.go` | Add `aad` parameter to `Encrypt`/`Decrypt`, add `DatabaseAAD` helper |
| `internal/crypto/encrypt_test.go` | Add tests for AAD encryption |
| `internal/store/databases.go` | Use AAD when encrypting/decrypting credentials |
| `internal/store/databases_test.go` | Update tests for new AAD behavior |

## Risks and Mitigations

### Risk: Breaking Existing Data

Existing databases encrypted without AAD will fail to decrypt with AAD.

**Mitigation**:
- Decrypt with fallback: try with AAD first, then without
- Provide migration function to re-encrypt existing data
- Log warning when using legacy (non-AAD) decryption

### Risk: ID Reuse After Delete

If a database is deleted and a new one gets the same ID, old ciphertext would theoretically decrypt.

**Mitigation**:
- This requires access to both old ciphertext AND current encryption key
- In practice, old ciphertext wouldn't exist in the table after delete
- Could add timestamp to AAD for extra safety: `database:42:1704067200`

## Success Criteria

1. New credentials are encrypted with AAD bound to database ID
2. Swapping `password_encrypted` between rows causes decryption failure
3. Existing credentials (without AAD) continue to work via fallback
4. All existing tests pass
5. New tests verify AAD binding behavior
