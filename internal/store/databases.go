package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/crypto"
)

// CreateDatabase creates a new database configuration.
// It uses a transaction to ensure the password is encrypted with AAD bound to the database UID.
func (s *Store) CreateDatabase(ctx context.Context, db *Database, encryptionKey []byte) (*Database, error) {
	plainPassword := db.Password

	result := &Database{
		Name:         db.Name,
		Description:  db.Description,
		Host:         db.Host,
		Port:         db.Port,
		DatabaseName: db.DatabaseName,
		Username:     db.Username,
		SSLMode:      db.SSLMode,
		CreatedBy:    db.CreatedBy,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	// Use a transaction to insert with a placeholder, get UID, then update with real encrypted password
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Insert with a temporary placeholder for password_encrypted
	// We'll update it immediately after getting the UID
	result.PasswordEncrypted = []byte("placeholder")
	_, err = tx.NewInsert().
		Model(result).
		Returning("*").
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create database: %w", err)
	}

	// Now encrypt password with AAD bound to the database UID
	aad := crypto.DatabaseAAD(result.UID.String())
	passwordEncrypted, err := crypto.Encrypt([]byte(plainPassword), encryptionKey, aad)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt password: %w", err)
	}

	// Update with the real encrypted password
	result.PasswordEncrypted = passwordEncrypted
	_, err = tx.NewUpdate().
		Model(result).
		Column("password_encrypted").
		WherePK().
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to update encrypted password: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return result, nil
}

// GetDatabaseByName retrieves a database by name
func (s *Store) GetDatabaseByName(ctx context.Context, name string) (*Database, error) {
	db := new(Database)
	err := s.db.NewSelect().
		Model(db).
		Where("name = ?", name).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrDatabaseNotFound
		}
		return nil, fmt.Errorf("failed to get database: %w", err)
	}
	return db, nil
}

// GetDatabaseByUID retrieves a database by UID
func (s *Store) GetDatabaseByUID(ctx context.Context, uid uuid.UUID) (*Database, error) {
	db := new(Database)
	err := s.db.NewSelect().
		Model(db).
		Where("uid = ?", uid).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrDatabaseNotFound
		}
		return nil, fmt.Errorf("failed to get database: %w", err)
	}
	return db, nil
}

// ListDatabases retrieves all databases
func (s *Store) ListDatabases(ctx context.Context) ([]Database, error) {
	var databases []Database
	err := s.db.NewSelect().
		Model(&databases).
		Order("name ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list databases: %w", err)
	}
	if databases == nil {
		databases = []Database{}
	}
	return databases, nil
}

// UpdateDatabase updates a database
func (s *Store) UpdateDatabase(ctx context.Context, uid uuid.UUID, updates DatabaseUpdate, encryptionKey []byte) error {
	q := s.db.NewUpdate().
		Model((*Database)(nil)).
		Where("uid = ?", uid).
		Set("updated_at = ?", time.Now())

	if updates.Description != nil {
		q = q.Set("description = ?", *updates.Description)
	}

	if updates.Host != nil {
		q = q.Set("host = ?", *updates.Host)
	}

	if updates.Port != nil {
		q = q.Set("port = ?", *updates.Port)
	}

	if updates.DatabaseName != nil {
		q = q.Set("database_name = ?", *updates.DatabaseName)
	}

	if updates.Username != nil {
		q = q.Set("username = ?", *updates.Username)
	}

	if updates.Password != nil {
		aad := crypto.DatabaseAAD(uid.String())
		passwordEncrypted, err := crypto.Encrypt([]byte(*updates.Password), encryptionKey, aad)
		if err != nil {
			return fmt.Errorf("failed to encrypt password: %w", err)
		}
		q = q.Set("password_encrypted = ?", passwordEncrypted)
	}

	if updates.SSLMode != nil {
		q = q.Set("ssl_mode = ?", *updates.SSLMode)
	}

	result, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to update database: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return ErrDatabaseNotFound
	}

	return nil
}

// DeleteDatabase deletes a database
func (s *Store) DeleteDatabase(ctx context.Context, uid uuid.UUID) error {
	result, err := s.db.NewDelete().
		Model((*Database)(nil)).
		Where("uid = ?", uid).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to delete database: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return ErrDatabaseNotFound
	}

	return nil
}

// DecryptPassword decrypts a database password using AAD bound to the database UID.
func (db *Database) DecryptPassword(encryptionKey []byte) error {
	aad := crypto.DatabaseAAD(db.UID.String())
	plaintext, err := crypto.Decrypt(db.PasswordEncrypted, encryptionKey, aad)
	if err != nil {
		return fmt.Errorf("failed to decrypt password: %w", err)
	}
	db.Password = string(plaintext)
	return nil
}
