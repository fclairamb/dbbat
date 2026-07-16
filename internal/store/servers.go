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

// CreateServer creates a new database configuration.
// It uses a transaction to ensure the password is encrypted with AAD bound to the database UID.
// Returns ErrTargetMatchesStorage if the target database matches the DBBat storage database.
func (s *Store) CreateServer(ctx context.Context, db *Server, encryptionKey []byte) (*Server, error) {
	// Security check: prevent configuring the storage database as a target
	if s.MatchesStorageDSN(db.Host, db.Port, db.DatabaseName) {
		return nil, ErrTargetMatchesStorage
	}

	plainPassword := db.Password

	result := &Server{
		Name:              db.Name,
		Description:       db.Description,
		Host:              db.Host,
		Port:              db.Port,
		DatabaseName:      db.DatabaseName,
		Username:          db.Username,
		SSLMode:           db.SSLMode,
		Protocol:          db.Protocol,
		OracleServiceName: db.OracleServiceName,
		ProtocolData:      db.ProtocolData,
		Listable:          db.Listable,
		CreatedBy:         db.CreatedBy,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
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
	aad := crypto.ServerAAD(result.UID.String())
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

// GetServerByName retrieves a database by name
func (s *Store) GetServerByName(ctx context.Context, name string) (*Server, error) {
	db := new(Server)
	err := s.db.NewSelect().
		Model(db).
		Where("name = ?", name).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrServerNotFound
		}
		return nil, fmt.Errorf("failed to get database: %w", err)
	}
	return db, nil
}

// GetServerByOracleServiceName retrieves an Oracle database by its service name.
//
// CAUTION: several dbbat databases may share one upstream service name (a
// mutualized Oracle instance); this returns an arbitrary matching row in that
// case. Resolution paths that must be deterministic should use
// ListServersByOracleServiceName and disambiguate explicitly.
func (s *Store) GetServerByOracleServiceName(ctx context.Context, serviceName string) (*Server, error) {
	db := new(Server)
	err := s.db.NewSelect().
		Model(db).
		Where("oracle_service_name = ?", serviceName).
		Where("protocol = ?", ProtocolOracle).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrServerNotFound
		}

		return nil, fmt.Errorf("failed to get database by oracle service name: %w", err)
	}

	return db, nil
}

// ListServersByOracleServiceName retrieves every Oracle database registered
// with the given upstream service name, ordered by name for determinism.
// Multiple dbbat logical databases can share one upstream SERVICE_NAME (e.g.
// several schemas of a mutualized Oracle instance behind MUTU01), so callers
// must handle 0, 1, or N results.
func (s *Store) ListServersByOracleServiceName(ctx context.Context, serviceName string) ([]Server, error) {
	var databases []Server

	err := s.db.NewSelect().
		Model(&databases).
		Where("oracle_service_name = ?", serviceName).
		Where("protocol = ?", ProtocolOracle).
		Order("name ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list databases by oracle service name: %w", err)
	}

	return databases, nil
}

// ListListableServers retrieves databases that are marked as listable.
// Used by the non-admin listing path so any authenticated user can discover
// databases available to request access to.
func (s *Store) ListListableServers(ctx context.Context) ([]Server, error) {
	var databases []Server
	err := s.db.NewSelect().
		Model(&databases).
		Where("listable = ?", true).
		Order("name ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list listable databases: %w", err)
	}
	if databases == nil {
		databases = []Server{}
	}
	return databases, nil
}

// GetServerByUID retrieves a database by UID
func (s *Store) GetServerByUID(ctx context.Context, uid uuid.UUID) (*Server, error) {
	db := new(Server)
	err := s.db.NewSelect().
		Model(db).
		Where("uid = ?", uid).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrServerNotFound
		}
		return nil, fmt.Errorf("failed to get database: %w", err)
	}
	return db, nil
}

// ListServers retrieves all databases
func (s *Store) ListServers(ctx context.Context) ([]Server, error) {
	var databases []Server
	err := s.db.NewSelect().
		Model(&databases).
		Order("name ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list databases: %w", err)
	}
	if databases == nil {
		databases = []Server{}
	}
	return databases, nil
}

// checkStorageDSNConflict verifies that a database update won't result in matching the storage DSN.
func (s *Store) checkStorageDSNConflict(ctx context.Context, uid uuid.UUID, updates ServerUpdate) error {
	if updates.Host == nil && updates.Port == nil && updates.DatabaseName == nil {
		return nil
	}

	current, err := s.GetServerByUID(ctx, uid)
	if err != nil {
		return err
	}

	host := valueOrDefault(updates.Host, current.Host)
	port := valueOrDefaultInt(updates.Port, current.Port)
	databaseName := valueOrDefault(updates.DatabaseName, current.DatabaseName)

	if s.MatchesStorageDSN(host, port, databaseName) {
		return ErrTargetMatchesStorage
	}
	return nil
}

func valueOrDefault(ptr *string, def string) string {
	if ptr != nil {
		return *ptr
	}
	return def
}

func valueOrDefaultInt(ptr *int, def int) int {
	if ptr != nil {
		return *ptr
	}
	return def
}

// UpdateServer updates a database.
// Returns ErrTargetMatchesStorage if the update would cause the target to match the DBBat storage database.
func (s *Store) UpdateServer(ctx context.Context, uid uuid.UUID, updates ServerUpdate, encryptionKey []byte) error {
	// Security check: if host, port, or database name are being updated,
	// verify the resulting configuration doesn't match the storage DSN
	if err := s.checkStorageDSNConflict(ctx, uid, updates); err != nil {
		return err
	}

	q := s.db.NewUpdate().
		Model((*Server)(nil)).
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
		aad := crypto.ServerAAD(uid.String())
		passwordEncrypted, err := crypto.Encrypt([]byte(*updates.Password), encryptionKey, aad)
		if err != nil {
			return fmt.Errorf("failed to encrypt password: %w", err)
		}
		q = q.Set("password_encrypted = ?", passwordEncrypted)
	}

	if updates.SSLMode != nil {
		q = q.Set("ssl_mode = ?", *updates.SSLMode)
	}

	if updates.Protocol != nil {
		q = q.Set("protocol = ?", *updates.Protocol)
	}

	if updates.OracleServiceName != nil {
		q = q.Set("oracle_service_name = ?", *updates.OracleServiceName)
	}

	if updates.MongoAuthSource != nil {
		// Merge into protocol_data.mongodb.auth_source rather than overwriting
		// the whole jsonb column, so other protocol_data keys survive.
		q = q.Set(
			"protocol_data = coalesce(protocol_data, '{}'::jsonb) || "+
				"jsonb_build_object('mongodb', coalesce(protocol_data->'mongodb', '{}'::jsonb) || "+
				"jsonb_build_object('auth_source', ?::text))",
			*updates.MongoAuthSource,
		)
	}

	if updates.Listable != nil {
		q = q.Set("listable = ?", *updates.Listable)
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
		return ErrServerNotFound
	}

	return nil
}

// DeleteServer deletes a database
func (s *Store) DeleteServer(ctx context.Context, uid uuid.UUID) error {
	result, err := s.db.NewDelete().
		Model((*Server)(nil)).
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
		return ErrServerNotFound
	}

	return nil
}

// MongoAuthSourceOrDefault returns the upstream MongoDB SCRAM authSource
// configured for this database, defaulting to "admin" (the MongoDB convention
// where service/root users are created) when unset.
func (db *Server) MongoAuthSourceOrDefault() string {
	if data := db.MongoData(); data != nil && data.AuthSource != "" {
		return data.AuthSource
	}

	return "admin"
}

// DecryptPassword decrypts a database password using AAD bound to the database UID.
func (db *Server) DecryptPassword(encryptionKey []byte) error {
	aad := crypto.ServerAAD(db.UID.String())
	plaintext, err := crypto.Decrypt(db.PasswordEncrypted, encryptionKey, aad)
	if err != nil {
		return fmt.Errorf("failed to decrypt password: %w", err)
	}
	db.Password = string(plaintext)
	return nil
}
