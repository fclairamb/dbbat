package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/crypto"
)

// testEncryptionKey returns a valid 32-byte AES key for testing.
func testEncryptionKey() []byte {
	return []byte("12345678901234567890123456789012") // 32 bytes
}

func TestCreateDatabase(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	// Create a user first (for created_by)
	user, err := store.CreateUser(ctx, "dbcreator", "hash", []string{RoleAdmin, RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	t.Run("create database", func(t *testing.T) {
		db := &Database{
			Name:         "testdb",
			Description:  "Test database",
			Host:         "localhost",
			Port:         5432,
			DatabaseName: "mydb",
			Username:     "dbuser",
			Password:     "secretpassword",
			SSLMode:      "prefer",
			CreatedBy:    &user.UID,
		}

		created, err := store.CreateDatabase(ctx, db, key)
		if err != nil {
			t.Fatalf("CreateDatabase() error = %v", err)
		}

		if created.UID == uuid.Nil {
			t.Error("CreateDatabase() db.UID = uuid.Nil")
		}
		if created.Name != "testdb" {
			t.Errorf("CreateDatabase() db.Name = %q, want %q", created.Name, "testdb")
		}
		if created.Host != "localhost" {
			t.Errorf("CreateDatabase() db.Host = %q, want %q", created.Host, "localhost")
		}
		if created.Port != 5432 {
			t.Errorf("CreateDatabase() db.Port = %d, want %d", created.Port, 5432)
		}
		if len(created.PasswordEncrypted) == 0 {
			t.Error("CreateDatabase() db.PasswordEncrypted is empty")
		}

		// Verify password is encrypted properly with AAD
		aad := crypto.DatabaseAAD(created.UID.String())
		decrypted, err := crypto.Decrypt(created.PasswordEncrypted, key, aad)
		if err != nil {
			t.Fatalf("Decrypt() error = %v", err)
		}
		if string(decrypted) != "secretpassword" {
			t.Errorf("decrypted password = %q, want %q", string(decrypted), "secretpassword")
		}
	})

	t.Run("duplicate name", func(t *testing.T) {
		db := &Database{
			Name:         "duplicate",
			Host:         "localhost",
			Port:         5432,
			DatabaseName: "db1",
			Username:     "user",
			Password:     "pass",
			SSLMode:      "disable",
		}

		_, err := store.CreateDatabase(ctx, db, key)
		if err != nil {
			t.Fatalf("CreateDatabase() first call error = %v", err)
		}

		db.DatabaseName = "db2"
		_, err = store.CreateDatabase(ctx, db, key)
		if err == nil {
			t.Error("CreateDatabase() expected error for duplicate name")
		}
	})
}

func TestGetDatabaseByName(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	// Create a test database
	db := &Database{
		Name:         "findbyname",
		Description:  "Find me by name",
		Host:         "dbhost",
		Port:         5433,
		DatabaseName: "targetdb",
		Username:     "targetuser",
		Password:     "targetpass",
		SSLMode:      "require",
	}
	created, err := store.CreateDatabase(ctx, db, key)
	if err != nil {
		t.Fatalf("CreateDatabase() error = %v", err)
	}

	t.Run("existing database", func(t *testing.T) {
		found, err := store.GetDatabaseByName(ctx, "findbyname")
		if err != nil {
			t.Fatalf("GetDatabaseByName() error = %v", err)
		}

		if found.UID != created.UID {
			t.Errorf("GetDatabaseByName() db.UID = %s, want %s", found.UID, created.UID)
		}
		if found.Host != "dbhost" {
			t.Errorf("GetDatabaseByName() db.Host = %q, want %q", found.Host, "dbhost")
		}
		if found.Port != 5433 {
			t.Errorf("GetDatabaseByName() db.Port = %d, want %d", found.Port, 5433)
		}
	})

	t.Run("non-existing database", func(t *testing.T) {
		_, err := store.GetDatabaseByName(ctx, "nonexistent")
		if !errors.Is(err, ErrDatabaseNotFound) {
			t.Errorf("GetDatabaseByName() error = %v, want %v", err, ErrDatabaseNotFound)
		}
	})
}

func TestGetDatabaseByUID(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	// Create a test database
	db := &Database{
		Name:         "findbyid",
		Host:         "localhost",
		Port:         5432,
		DatabaseName: "db",
		Username:     "user",
		Password:     "pass",
		SSLMode:      "disable",
	}
	created, err := store.CreateDatabase(ctx, db, key)
	if err != nil {
		t.Fatalf("CreateDatabase() error = %v", err)
	}

	t.Run("existing database", func(t *testing.T) {
		found, err := store.GetDatabaseByUID(ctx, created.UID)
		if err != nil {
			t.Fatalf("GetDatabaseByUID() error = %v", err)
		}

		if found.Name != "findbyid" {
			t.Errorf("GetDatabaseByUID() db.Name = %q, want %q", found.Name, "findbyid")
		}
	})

	t.Run("non-existing database", func(t *testing.T) {
		_, err := store.GetDatabaseByUID(ctx, uuid.New())
		if !errors.Is(err, ErrDatabaseNotFound) {
			t.Errorf("GetDatabaseByUID() error = %v, want %v", err, ErrDatabaseNotFound)
		}
	})
}

func TestListDatabases(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	t.Run("empty list", func(t *testing.T) {
		dbs, err := store.ListDatabases(ctx)
		if err != nil {
			t.Fatalf("ListDatabases() error = %v", err)
		}
		if len(dbs) != 0 {
			t.Errorf("ListDatabases() len = %d, want 0", len(dbs))
		}
	})

	// Create some databases
	for _, name := range []string{"db_alpha", "db_beta"} {
		db := &Database{
			Name:         name,
			Host:         "localhost",
			Port:         5432,
			DatabaseName: name,
			Username:     "user",
			Password:     "pass",
			SSLMode:      "disable",
		}
		_, err := store.CreateDatabase(ctx, db, key)
		if err != nil {
			t.Fatalf("CreateDatabase() error = %v", err)
		}
	}

	t.Run("with databases", func(t *testing.T) {
		dbs, err := store.ListDatabases(ctx)
		if err != nil {
			t.Fatalf("ListDatabases() error = %v", err)
		}
		if len(dbs) != 2 {
			t.Errorf("ListDatabases() len = %d, want 2", len(dbs))
		}

		// Should be ordered by name
		if dbs[0].Name != "db_alpha" {
			t.Errorf("ListDatabases()[0].Name = %q, want %q", dbs[0].Name, "db_alpha")
		}
		if dbs[1].Name != "db_beta" {
			t.Errorf("ListDatabases()[1].Name = %q, want %q", dbs[1].Name, "db_beta")
		}
	})
}

func TestUpdateDatabase(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	// Create a test database
	db := &Database{
		Name:         "toupdate",
		Description:  "Original description",
		Host:         "oldhost",
		Port:         5432,
		DatabaseName: "olddb",
		Username:     "olduser",
		Password:     "oldpass",
		SSLMode:      "disable",
	}
	created, err := store.CreateDatabase(ctx, db, key)
	if err != nil {
		t.Fatalf("CreateDatabase() error = %v", err)
	}

	t.Run("update description", func(t *testing.T) {
		newDesc := "New description"
		err := store.UpdateDatabase(ctx, created.UID, DatabaseUpdate{Description: &newDesc}, key)
		if err != nil {
			t.Fatalf("UpdateDatabase() error = %v", err)
		}

		found, _ := store.GetDatabaseByUID(ctx, created.UID)
		if found.Description != "New description" {
			t.Errorf("db.Description = %q, want %q", found.Description, "New description")
		}
	})

	t.Run("update host and port", func(t *testing.T) {
		newHost := "newhost"
		newPort := 5433
		err := store.UpdateDatabase(ctx, created.UID, DatabaseUpdate{Host: &newHost, Port: &newPort}, key)
		if err != nil {
			t.Fatalf("UpdateDatabase() error = %v", err)
		}

		found, _ := store.GetDatabaseByUID(ctx, created.UID)
		if found.Host != "newhost" {
			t.Errorf("db.Host = %q, want %q", found.Host, "newhost")
		}
		if found.Port != 5433 {
			t.Errorf("db.Port = %d, want %d", found.Port, 5433)
		}
	})

	t.Run("update password", func(t *testing.T) {
		newPass := "newsecretpass"
		err := store.UpdateDatabase(ctx, created.UID, DatabaseUpdate{Password: &newPass}, key)
		if err != nil {
			t.Fatalf("UpdateDatabase() error = %v", err)
		}

		found, _ := store.GetDatabaseByUID(ctx, created.UID)
		err = found.DecryptPassword(key)
		if err != nil {
			t.Fatalf("DecryptPassword() error = %v", err)
		}
		if found.Password != "newsecretpass" {
			t.Errorf("db.Password = %q, want %q", found.Password, "newsecretpass")
		}
	})

	t.Run("non-existing database", func(t *testing.T) {
		newHost := "host"
		err := store.UpdateDatabase(ctx, uuid.New(), DatabaseUpdate{Host: &newHost}, key)
		if !errors.Is(err, ErrDatabaseNotFound) {
			t.Errorf("UpdateDatabase() error = %v, want %v", err, ErrDatabaseNotFound)
		}
	})
}

func TestDeleteDatabase(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	// Create a test database
	db := &Database{
		Name:         "todelete",
		Host:         "localhost",
		Port:         5432,
		DatabaseName: "db",
		Username:     "user",
		Password:     "pass",
		SSLMode:      "disable",
	}
	created, err := store.CreateDatabase(ctx, db, key)
	if err != nil {
		t.Fatalf("CreateDatabase() error = %v", err)
	}

	t.Run("delete existing database", func(t *testing.T) {
		err := store.DeleteDatabase(ctx, created.UID)
		if err != nil {
			t.Fatalf("DeleteDatabase() error = %v", err)
		}

		_, err = store.GetDatabaseByUID(ctx, created.UID)
		if !errors.Is(err, ErrDatabaseNotFound) {
			t.Errorf("GetDatabaseByUID() error = %v, want %v", err, ErrDatabaseNotFound)
		}
	})

	t.Run("delete non-existing database", func(t *testing.T) {
		err := store.DeleteDatabase(ctx, uuid.New())
		if !errors.Is(err, ErrDatabaseNotFound) {
			t.Errorf("DeleteDatabase() error = %v, want %v", err, ErrDatabaseNotFound)
		}
	})
}

func TestDecryptPassword(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	// Create a test database
	db := &Database{
		Name:         "decrypttest",
		Host:         "localhost",
		Port:         5432,
		DatabaseName: "db",
		Username:     "user",
		Password:     "mysecretpassword",
		SSLMode:      "disable",
	}
	created, err := store.CreateDatabase(ctx, db, key)
	if err != nil {
		t.Fatalf("CreateDatabase() error = %v", err)
	}

	t.Run("decrypt password", func(t *testing.T) {
		// Password should be empty before decryption
		if created.Password != "" {
			t.Errorf("db.Password = %q, want empty before DecryptPassword", created.Password)
		}

		err := created.DecryptPassword(key)
		if err != nil {
			t.Fatalf("DecryptPassword() error = %v", err)
		}

		if created.Password != "mysecretpassword" {
			t.Errorf("db.Password = %q, want %q", created.Password, "mysecretpassword")
		}
	})

	t.Run("decrypt with wrong key", func(t *testing.T) {
		wrongKey := []byte("wrongkey1234567890123456789012")
		err := created.DecryptPassword(wrongKey)
		if err == nil {
			t.Error("DecryptPassword() expected error with wrong key")
		}
	})
}
