package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

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

	// Validate the SSH tunnel reference before doing any work.
	if db.ViaUID != nil {
		if err := s.validateViaUID(ctx, uuid.Nil, *db.ViaUID); err != nil {
			return nil, err
		}
	}

	plainPassword := db.Password

	// Capture plaintext SSH secrets (if any) before they are cleared; they are
	// encrypted after the UID is known, exactly like the password.
	var sshPlain *SSHServerData
	if sd := db.SSHData(); sd != nil && (sd.PrivateKey != "" || sd.Passphrase != "") {
		sshPlain = sd
	}

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
		ViaUID:            db.ViaUID,
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

	// Encrypt SSH secrets (AAD-bound to the UID) and persist protocol_data.
	if sshPlain != nil {
		if err := encryptSSHSecrets(result.UID, result.ProtocolData.SSH, encryptionKey); err != nil {
			return nil, err
		}
		if _, err := tx.NewUpdate().
			Model(result).
			Column("protocol_data").
			WherePK().
			Exec(ctx); err != nil {
			return nil, fmt.Errorf("failed to persist ssh secrets: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return result, nil
}

// encryptSSHSecrets encrypts the plaintext PrivateKey/Passphrase on sd in place
// (AAD-bound to the server UID) and clears the plaintext fields, mirroring the
// password-encryption pattern.
func encryptSSHSecrets(uid uuid.UUID, sd *SSHServerData, encryptionKey []byte) error {
	aad := crypto.ServerAAD(uid.String())
	if sd.PrivateKey != "" {
		enc, err := crypto.Encrypt([]byte(sd.PrivateKey), encryptionKey, aad)
		if err != nil {
			return fmt.Errorf("failed to encrypt ssh private key: %w", err)
		}
		sd.PrivateKeyEncrypted = enc
		sd.PrivateKey = ""
	}
	if sd.Passphrase != "" {
		enc, err := crypto.Encrypt([]byte(sd.Passphrase), encryptionKey, aad)
		if err != nil {
			return fmt.Errorf("failed to encrypt ssh passphrase: %w", err)
		}
		sd.PassphraseEncrypted = enc
		sd.Passphrase = ""
	}
	return nil
}

// DecryptSSHSecrets decrypts the SSH private key and passphrase into the
// in-memory PrivateKey/Passphrase fields (AAD-bound to the server UID). No-op
// when the server has no SSH material.
func (db *Server) DecryptSSHSecrets(encryptionKey []byte) error {
	sd := db.SSHData()
	if sd == nil {
		return nil
	}
	aad := crypto.ServerAAD(db.UID.String())
	if len(sd.PrivateKeyEncrypted) > 0 {
		pt, err := crypto.Decrypt(sd.PrivateKeyEncrypted, encryptionKey, aad)
		if err != nil {
			return fmt.Errorf("failed to decrypt ssh private key: %w", err)
		}
		sd.PrivateKey = string(pt)
	}
	if len(sd.PassphraseEncrypted) > 0 {
		pt, err := crypto.Decrypt(sd.PassphraseEncrypted, encryptionKey, aad)
		if err != nil {
			return fmt.Errorf("failed to decrypt ssh passphrase: %w", err)
		}
		sd.Passphrase = string(pt)
	}
	return nil
}

// SetKnownHostKey persists the TOFU-learned SSH host key for an SSH server row,
// merging into protocol_data.ssh.known_host_key without disturbing other keys.
func (s *Store) SetKnownHostKey(ctx context.Context, uid uuid.UUID, hostKey string) error {
	_, err := s.db.NewUpdate().
		Model((*Server)(nil)).
		Where("uid = ?", uid).
		Set("protocol_data = coalesce(protocol_data, '{}'::jsonb) || "+
			"jsonb_build_object('ssh', coalesce(protocol_data->'ssh', '{}'::jsonb) || "+
			"jsonb_build_object('known_host_key', ?::text))", hostKey).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to store ssh host key: %w", err)
	}
	return nil
}

// validateViaUID verifies that viaUID references an existing SSH server and
// that the via chain neither loops nor passes back through selfUID (pass
// uuid.Nil for selfUID on create, when the row has no UID yet).
func (s *Store) validateViaUID(ctx context.Context, selfUID, viaUID uuid.UUID) error {
	seen := map[uuid.UUID]bool{}
	cur := viaUID
	for {
		if selfUID != uuid.Nil && cur == selfUID {
			return ErrServerViaCycle
		}
		if seen[cur] {
			return ErrServerViaCycle
		}
		seen[cur] = true

		via, err := s.GetServerByUID(ctx, cur)
		if err != nil {
			return err
		}
		if via.Protocol != ProtocolSSH {
			return ErrServerViaNotSSH
		}
		if via.ViaUID == nil {
			return nil
		}
		cur = *via.ViaUID
	}
}

// ListSSHServers returns every SSH bastion row (protocol = 'ssh'), for the
// admin SSH-server management view and the "via SSH server" selector. These
// rows are excluded from every grantable/connectable target listing.
func (s *Store) ListSSHServers(ctx context.Context) ([]Server, error) {
	var servers []Server
	err := s.db.NewSelect().
		Model(&servers).
		Where("protocol = ?", ProtocolSSH).
		Order("name ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list ssh servers: %w", err)
	}
	if servers == nil {
		servers = []Server{}
	}
	return servers, nil
}

// GetServerByName retrieves a database by name
func (s *Store) GetServerByName(ctx context.Context, name string) (*Server, error) {
	db := new(Server)
	err := s.db.NewSelect().
		Model(db).
		Where("name = ?", name).
		// Targets only: an SSH bastion is a dial path, never connectable by name.
		Where("protocol <> ?", ProtocolSSH).
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
		// Targets only: SSH bastions are never grantable/listable targets.
		Where("protocol <> ?", ProtocolSSH).
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

// ListServers retrieves all database *targets* (every protocol except 'ssh').
// SSH bastions are managed separately via ListSSHServers so they never leak
// into grantable/connectable target contexts (dropdowns, admin database list).
func (s *Store) ListServers(ctx context.Context) ([]Server, error) {
	var databases []Server
	err := s.db.NewSelect().
		Model(&databases).
		Where("protocol <> ?", ProtocolSSH).
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

	// Validate a new via_uid (must reference an ssh row, no cycle through self).
	if updates.ViaUID != nil {
		if err := s.validateViaUID(ctx, uid, *updates.ViaUID); err != nil {
			return err
		}
	}

	q := s.db.NewUpdate().
		Model((*Server)(nil)).
		Where("uid = ?", uid).
		Set("updated_at = ?", time.Now())

	q = applyServerColumnUpdates(q, updates)

	if updates.Password != nil {
		aad := crypto.ServerAAD(uid.String())
		passwordEncrypted, err := crypto.Encrypt([]byte(*updates.Password), encryptionKey, aad)
		if err != nil {
			return fmt.Errorf("failed to encrypt password: %w", err)
		}
		q = q.Set("password_encrypted = ?", passwordEncrypted)
	}

	// SSH secrets: encrypt (AAD-bound to the UID) and write the merged
	// protocol_data, preserving any other protocol_data keys.
	if updates.SSHPrivateKey != nil || updates.SSHPassphrase != nil {
		pd, err := s.mergedSSHSecrets(ctx, uid, updates, encryptionKey)
		if err != nil {
			return err
		}
		q = q.Set("protocol_data = ?", pd)
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

// applyServerColumnUpdates adds the plain (non-encrypted) column setters to an
// update query. Password and SSH-secret columns are handled by the caller,
// which needs the encryption key.
func applyServerColumnUpdates(q *bun.UpdateQuery, updates ServerUpdate) *bun.UpdateQuery {
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
	if updates.ClearViaUID {
		q = q.Set("via_uid = NULL")
	} else if updates.ViaUID != nil {
		q = q.Set("via_uid = ?", *updates.ViaUID)
	}
	return q
}

// mergedSSHSecrets loads the server's current protocol_data, encrypts any
// provided SSH secrets (AAD-bound to the UID) into protocol_data.ssh, and
// returns the merged struct — preserving other protocol_data keys.
func (s *Store) mergedSSHSecrets(ctx context.Context, uid uuid.UUID, updates ServerUpdate, encryptionKey []byte) (*ServerProtocolData, error) {
	current, err := s.GetServerByUID(ctx, uid)
	if err != nil {
		return nil, err
	}
	if current.ProtocolData == nil {
		current.ProtocolData = &ServerProtocolData{}
	}
	if current.ProtocolData.SSH == nil {
		current.ProtocolData.SSH = &SSHServerData{}
	}
	sd := current.ProtocolData.SSH
	aad := crypto.ServerAAD(uid.String())

	if updates.SSHPrivateKey != nil {
		enc, err := crypto.Encrypt([]byte(*updates.SSHPrivateKey), encryptionKey, aad)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt ssh private key: %w", err)
		}
		sd.PrivateKeyEncrypted = enc
	}
	if updates.SSHPassphrase != nil {
		enc, err := crypto.Encrypt([]byte(*updates.SSHPassphrase), encryptionKey, aad)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt ssh passphrase: %w", err)
		}
		sd.PassphraseEncrypted = enc
	}
	return current.ProtocolData, nil
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
