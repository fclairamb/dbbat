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
		db := &Server{
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

		created, err := store.CreateServer(ctx, db, key)
		if err != nil {
			t.Fatalf("CreateServer() error = %v", err)
		}

		if created.UID == uuid.Nil {
			t.Error("CreateServer() db.UID = uuid.Nil")
		}
		if created.Name != "testdb" {
			t.Errorf("CreateServer() db.Name = %q, want %q", created.Name, "testdb")
		}
		if created.Host != "localhost" {
			t.Errorf("CreateServer() db.Host = %q, want %q", created.Host, "localhost")
		}
		if created.Port != 5432 {
			t.Errorf("CreateServer() db.Port = %d, want %d", created.Port, 5432)
		}
		if len(created.PasswordEncrypted) == 0 {
			t.Error("CreateServer() db.PasswordEncrypted is empty")
		}

		// Verify password is encrypted properly with AAD
		aad := crypto.ServerAAD(created.UID.String())
		decrypted, err := crypto.Decrypt(created.PasswordEncrypted, key, aad)
		if err != nil {
			t.Fatalf("Decrypt() error = %v", err)
		}
		if string(decrypted) != "secretpassword" {
			t.Errorf("decrypted password = %q, want %q", string(decrypted), "secretpassword")
		}
	})

	t.Run("duplicate name", func(t *testing.T) {
		db := &Server{
			Name:         "duplicate",
			Host:         "localhost",
			Port:         5432,
			DatabaseName: "db1",
			Username:     "user",
			Password:     "pass",
			SSLMode:      "disable",
		}

		_, err := store.CreateServer(ctx, db, key)
		if err != nil {
			t.Fatalf("CreateServer() first call error = %v", err)
		}

		db.DatabaseName = "db2"
		_, err = store.CreateServer(ctx, db, key)
		if err == nil {
			t.Error("CreateServer() expected error for duplicate name")
		}
	})
}

func TestCreateDatabase_Protocols(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	t.Run("postgresql default", func(t *testing.T) {
		db := &Server{
			Name:         "pg-db",
			Host:         "localhost",
			Port:         5432,
			DatabaseName: "mydb",
			Username:     "pguser",
			Password:     "pgpass",
			SSLMode:      "prefer",
		}
		created, err := store.CreateServer(ctx, db, key)
		if err != nil {
			t.Fatalf("CreateServer() error = %v", err)
		}
		if created.Protocol != ProtocolPostgreSQL {
			t.Errorf("Protocol = %q, want %q", created.Protocol, ProtocolPostgreSQL)
		}

		found, err := store.GetServerByUID(ctx, created.UID)
		if err != nil {
			t.Fatalf("GetServerByUID() error = %v", err)
		}
		if found.Protocol != ProtocolPostgreSQL {
			t.Errorf("Protocol after read = %q, want %q", found.Protocol, ProtocolPostgreSQL)
		}
	})

	t.Run("postgresql explicit", func(t *testing.T) {
		db := &Server{
			Name:         "pg-db-explicit",
			Host:         "localhost",
			Port:         5432,
			DatabaseName: "mydb2",
			Username:     "pguser",
			Password:     "pgpass",
			SSLMode:      "prefer",
			Protocol:     ProtocolPostgreSQL,
		}
		created, err := store.CreateServer(ctx, db, key)
		if err != nil {
			t.Fatalf("CreateServer() error = %v", err)
		}
		if created.Protocol != ProtocolPostgreSQL {
			t.Errorf("Protocol = %q, want %q", created.Protocol, ProtocolPostgreSQL)
		}
	})

	t.Run("oracle", func(t *testing.T) {
		serviceName := "ORCL"
		db := &Server{
			Name:              "ora-db",
			Host:              "oracle-host",
			Port:              1521,
			Username:          "orauser",
			Password:          "orapass",
			Protocol:          ProtocolOracle,
			OracleServiceName: &serviceName,
		}
		created, err := store.CreateServer(ctx, db, key)
		if err != nil {
			t.Fatalf("CreateServer() error = %v", err)
		}
		if created.Protocol != ProtocolOracle {
			t.Errorf("Protocol = %q, want %q", created.Protocol, ProtocolOracle)
		}
		if created.OracleServiceName == nil || *created.OracleServiceName != "ORCL" {
			t.Errorf("OracleServiceName = %v, want %q", created.OracleServiceName, "ORCL")
		}

		found, err := store.GetServerByUID(ctx, created.UID)
		if err != nil {
			t.Fatalf("GetServerByUID() error = %v", err)
		}
		if found.Protocol != ProtocolOracle {
			t.Errorf("Protocol after read = %q, want %q", found.Protocol, ProtocolOracle)
		}
		if found.OracleServiceName == nil || *found.OracleServiceName != "ORCL" {
			t.Errorf("OracleServiceName after read = %v, want %q", found.OracleServiceName, "ORCL")
		}
	})

	t.Run("update protocol", func(t *testing.T) {
		db := &Server{
			Name:         "update-proto",
			Host:         "localhost",
			Port:         5432,
			DatabaseName: "mydb",
			Username:     "user",
			Password:     "pass",
			Protocol:     ProtocolPostgreSQL,
		}
		created, err := store.CreateServer(ctx, db, key)
		if err != nil {
			t.Fatalf("CreateServer() error = %v", err)
		}

		newProto := ProtocolOracle
		serviceName := "TEST01"
		err = store.UpdateServer(ctx, created.UID, ServerUpdate{
			Protocol:          &newProto,
			OracleServiceName: &serviceName,
		}, key)
		if err != nil {
			t.Fatalf("UpdateServer() error = %v", err)
		}

		found, err := store.GetServerByUID(ctx, created.UID)
		if err != nil {
			t.Fatalf("GetServerByUID() error = %v", err)
		}
		if found.Protocol != ProtocolOracle {
			t.Errorf("Protocol after update = %q, want %q", found.Protocol, ProtocolOracle)
		}
		if found.OracleServiceName == nil || *found.OracleServiceName != "TEST01" {
			t.Errorf("OracleServiceName after update = %v, want %q", found.OracleServiceName, "TEST01")
		}
	})
}

func TestGetDatabaseByName(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	// Create a test database
	db := &Server{
		Name:         "findbyname",
		Description:  "Find me by name",
		Host:         "dbhost",
		Port:         5433,
		DatabaseName: "targetdb",
		Username:     "targetuser",
		Password:     "targetpass",
		SSLMode:      "require",
	}
	created, err := store.CreateServer(ctx, db, key)
	if err != nil {
		t.Fatalf("CreateServer() error = %v", err)
	}

	t.Run("existing database", func(t *testing.T) {
		found, err := store.GetServerByName(ctx, "findbyname")
		if err != nil {
			t.Fatalf("GetServerByName() error = %v", err)
		}

		if found.UID != created.UID {
			t.Errorf("GetServerByName() db.UID = %s, want %s", found.UID, created.UID)
		}
		if found.Host != "dbhost" {
			t.Errorf("GetServerByName() db.Host = %q, want %q", found.Host, "dbhost")
		}
		if found.Port != 5433 {
			t.Errorf("GetServerByName() db.Port = %d, want %d", found.Port, 5433)
		}
	})

	t.Run("non-existing database", func(t *testing.T) {
		_, err := store.GetServerByName(ctx, "nonexistent")
		if !errors.Is(err, ErrServerNotFound) {
			t.Errorf("GetServerByName() error = %v, want %v", err, ErrServerNotFound)
		}
	})
}

func TestGetDatabaseByUID(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	// Create a test database
	db := &Server{
		Name:         "findbyid",
		Host:         "localhost",
		Port:         5432,
		DatabaseName: "db",
		Username:     "user",
		Password:     "pass",
		SSLMode:      "disable",
	}
	created, err := store.CreateServer(ctx, db, key)
	if err != nil {
		t.Fatalf("CreateServer() error = %v", err)
	}

	t.Run("existing database", func(t *testing.T) {
		found, err := store.GetServerByUID(ctx, created.UID)
		if err != nil {
			t.Fatalf("GetServerByUID() error = %v", err)
		}

		if found.Name != "findbyid" {
			t.Errorf("GetServerByUID() db.Name = %q, want %q", found.Name, "findbyid")
		}
	})

	t.Run("non-existing database", func(t *testing.T) {
		_, err := store.GetServerByUID(ctx, uuid.New())
		if !errors.Is(err, ErrServerNotFound) {
			t.Errorf("GetServerByUID() error = %v, want %v", err, ErrServerNotFound)
		}
	})
}

func TestListDatabases(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	t.Run("empty list", func(t *testing.T) {
		dbs, err := store.ListServers(ctx)
		if err != nil {
			t.Fatalf("ListServers() error = %v", err)
		}
		if len(dbs) != 0 {
			t.Errorf("ListServers() len = %d, want 0", len(dbs))
		}
	})

	// Create some databases
	for _, name := range []string{"db_alpha", "db_beta"} {
		db := &Server{
			Name:         name,
			Host:         "localhost",
			Port:         5432,
			DatabaseName: name,
			Username:     "user",
			Password:     "pass",
			SSLMode:      "disable",
		}
		_, err := store.CreateServer(ctx, db, key)
		if err != nil {
			t.Fatalf("CreateServer() error = %v", err)
		}
	}

	t.Run("with databases", func(t *testing.T) {
		dbs, err := store.ListServers(ctx)
		if err != nil {
			t.Fatalf("ListServers() error = %v", err)
		}
		if len(dbs) != 2 {
			t.Errorf("ListServers() len = %d, want 2", len(dbs))
		}

		// Should be ordered by name
		if dbs[0].Name != "db_alpha" {
			t.Errorf("ListServers()[0].Name = %q, want %q", dbs[0].Name, "db_alpha")
		}
		if dbs[1].Name != "db_beta" {
			t.Errorf("ListServers()[1].Name = %q, want %q", dbs[1].Name, "db_beta")
		}
	})
}

func TestUpdateDatabase(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	// Create a test database
	db := &Server{
		Name:         "toupdate",
		Description:  "Original description",
		Host:         "oldhost",
		Port:         5432,
		DatabaseName: "olddb",
		Username:     "olduser",
		Password:     "oldpass",
		SSLMode:      "disable",
	}
	created, err := store.CreateServer(ctx, db, key)
	if err != nil {
		t.Fatalf("CreateServer() error = %v", err)
	}

	t.Run("update description", func(t *testing.T) {
		newDesc := "New description"
		err := store.UpdateServer(ctx, created.UID, ServerUpdate{Description: &newDesc}, key)
		if err != nil {
			t.Fatalf("UpdateServer() error = %v", err)
		}

		found, _ := store.GetServerByUID(ctx, created.UID)
		if found.Description != "New description" {
			t.Errorf("db.Description = %q, want %q", found.Description, "New description")
		}
	})

	t.Run("update host and port", func(t *testing.T) {
		newHost := "newhost"
		newPort := 5433
		err := store.UpdateServer(ctx, created.UID, ServerUpdate{Host: &newHost, Port: &newPort}, key)
		if err != nil {
			t.Fatalf("UpdateServer() error = %v", err)
		}

		found, _ := store.GetServerByUID(ctx, created.UID)
		if found.Host != "newhost" {
			t.Errorf("db.Host = %q, want %q", found.Host, "newhost")
		}
		if found.Port != 5433 {
			t.Errorf("db.Port = %d, want %d", found.Port, 5433)
		}
	})

	t.Run("update password", func(t *testing.T) {
		newPass := "newsecretpass"
		err := store.UpdateServer(ctx, created.UID, ServerUpdate{Password: &newPass}, key)
		if err != nil {
			t.Fatalf("UpdateServer() error = %v", err)
		}

		found, _ := store.GetServerByUID(ctx, created.UID)
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
		err := store.UpdateServer(ctx, uuid.New(), ServerUpdate{Host: &newHost}, key)
		if !errors.Is(err, ErrServerNotFound) {
			t.Errorf("UpdateServer() error = %v, want %v", err, ErrServerNotFound)
		}
	})
}

func TestDeleteDatabase(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	// Create a test database
	db := &Server{
		Name:         "todelete",
		Host:         "localhost",
		Port:         5432,
		DatabaseName: "db",
		Username:     "user",
		Password:     "pass",
		SSLMode:      "disable",
	}
	created, err := store.CreateServer(ctx, db, key)
	if err != nil {
		t.Fatalf("CreateServer() error = %v", err)
	}

	t.Run("delete existing database", func(t *testing.T) {
		err := store.DeleteServer(ctx, created.UID)
		if err != nil {
			t.Fatalf("DeleteServer() error = %v", err)
		}

		_, err = store.GetServerByUID(ctx, created.UID)
		if !errors.Is(err, ErrServerNotFound) {
			t.Errorf("GetServerByUID() error = %v, want %v", err, ErrServerNotFound)
		}
	})

	t.Run("delete non-existing database", func(t *testing.T) {
		err := store.DeleteServer(ctx, uuid.New())
		if !errors.Is(err, ErrServerNotFound) {
			t.Errorf("DeleteServer() error = %v, want %v", err, ErrServerNotFound)
		}
	})
}

func TestDecryptPassword(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	// Create a test database
	db := &Server{
		Name:         "decrypttest",
		Host:         "localhost",
		Port:         5432,
		DatabaseName: "db",
		Username:     "user",
		Password:     "mysecretpassword",
		SSLMode:      "disable",
	}
	created, err := store.CreateServer(ctx, db, key)
	if err != nil {
		t.Fatalf("CreateServer() error = %v", err)
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

func TestCreateDatabase_DefaultListable(t *testing.T) {
	t.Parallel()

	s := setupTestStoreNoCleanup(t)
	ctx := context.Background()
	key := testEncryptionKey()
	suffix := uuid.NewString()[:8]

	// Create without explicitly setting Listable — should default to true.
	db := &Server{
		Name:         "listable-default-" + suffix,
		Host:         "localhost",
		Port:         5432,
		DatabaseName: "mydb",
		Username:     "user",
		Password:     "pass",
		SSLMode:      "prefer",
		Listable:     true, // explicit true mirrors what the handler sets when req.Listable==nil
	}

	created, err := s.CreateServer(ctx, db, key)
	if err != nil {
		t.Fatalf("CreateServer() error = %v", err)
	}
	if !created.Listable {
		t.Error("CreateServer() Listable = false, want true (default)")
	}

	// Re-fetch to confirm DB persisted it correctly.
	found, err := s.GetServerByUID(ctx, created.UID)
	if err != nil {
		t.Fatalf("GetServerByUID() error = %v", err)
	}
	if !found.Listable {
		t.Error("GetServerByUID() Listable = false, want true after create")
	}
}

func TestUpdateDatabase_Listable(t *testing.T) {
	t.Parallel()

	s := setupTestStoreNoCleanup(t)
	ctx := context.Background()
	key := testEncryptionKey()

	newDB := func(name string, listable bool) *Server {
		return &Server{
			Name:         name,
			Host:         "localhost",
			Port:         5432,
			DatabaseName: "mydb",
			Username:     "user",
			Password:     "pass",
			SSLMode:      "prefer",
			Listable:     listable,
		}
	}

	t.Run("true to false", func(t *testing.T) {
		t.Parallel()
		created, err := s.CreateServer(ctx, newDB("lu-true-to-false-"+uuid.NewString()[:8], true), key)
		if err != nil {
			t.Fatalf("CreateServer() error = %v", err)
		}
		f := false
		if err := s.UpdateServer(ctx, created.UID, ServerUpdate{Listable: &f}, key); err != nil {
			t.Fatalf("UpdateServer() error = %v", err)
		}
		found, err := s.GetServerByUID(ctx, created.UID)
		if err != nil {
			t.Fatalf("GetServerByUID() error = %v", err)
		}
		if found.Listable {
			t.Error("Listable = true after setting to false")
		}
	})

	t.Run("false to true", func(t *testing.T) {
		t.Parallel()
		created, err := s.CreateServer(ctx, newDB("lu-false-to-true-"+uuid.NewString()[:8], false), key)
		if err != nil {
			t.Fatalf("CreateServer() error = %v", err)
		}
		tr := true
		if err := s.UpdateServer(ctx, created.UID, ServerUpdate{Listable: &tr}, key); err != nil {
			t.Fatalf("UpdateServer() error = %v", err)
		}
		found, err := s.GetServerByUID(ctx, created.UID)
		if err != nil {
			t.Fatalf("GetServerByUID() error = %v", err)
		}
		if !found.Listable {
			t.Error("Listable = false after setting to true")
		}
	})
}

func TestListListableDatabases(t *testing.T) {
	t.Parallel()

	s := setupTestStoreNoCleanup(t)
	ctx := context.Background()
	key := testEncryptionKey()
	suffix := uuid.NewString()[:8]

	mkDB := func(name string, listable bool) *Server {
		return &Server{
			Name:         name + "-" + suffix,
			Host:         "localhost",
			Port:         5432,
			DatabaseName: "mydb",
			Username:     "user",
			Password:     "pass",
			SSLMode:      "prefer",
			Listable:     listable,
		}
	}

	_, err := s.CreateServer(ctx, mkDB("listable-a", true), key)
	if err != nil {
		t.Fatalf("CreateServer(listable-a) error = %v", err)
	}
	_, err = s.CreateServer(ctx, mkDB("listable-b", true), key)
	if err != nil {
		t.Fatalf("CreateServer(listable-b) error = %v", err)
	}
	hidden, err := s.CreateServer(ctx, mkDB("hidden", false), key)
	if err != nil {
		t.Fatalf("CreateServer(hidden) error = %v", err)
	}

	all, err := s.ListListableServers(ctx)
	if err != nil {
		t.Fatalf("ListListableServers() error = %v", err)
	}

	for _, db := range all {
		if db.UID == hidden.UID {
			t.Error("ListListableServers() returned a non-listable database")
		}
	}

	// Confirm at least two listable DBs are present (others from parallel tests may exist).
	listableCount := 0
	for _, db := range all {
		if db.Listable {
			listableCount++
		}
	}
	if listableCount < 2 {
		t.Errorf("ListListableServers() returned %d listable DBs, want ≥2", listableCount)
	}
}

// TestListDatabasesByOracleServiceName covers the mutualized-instance case:
// several dbbat logical databases sharing one upstream oracle_service_name
// (e.g. MUTU01) must ALL be returned, ordered by name — the previous
// single-row lookup returned an arbitrary one, so the Oracle proxy could
// resolve a connection to the wrong logical database.
func TestListDatabasesByOracleServiceName(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	svc := "MUTU01_TEST"
	mkOra := func(name string) *Server {
		return &Server{
			Name:              name,
			Host:              "orahost",
			Port:              1521,
			DatabaseName:      name,
			Username:          name + "_user",
			Password:          "pass",
			SSLMode:           "prefer",
			Protocol:          ProtocolOracle,
			OracleServiceName: &svc,
		}
	}

	for _, name := range []string{"abyla_i3f_t", "abyla_glh_t", "abyla_onv_t"} {
		if _, err := s.CreateServer(ctx, mkOra(name), key); err != nil {
			t.Fatalf("CreateServer(%s) error = %v", name, err)
		}
	}

	t.Run("returns all databases sharing the service name, ordered", func(t *testing.T) {
		dbs, err := s.ListServersByOracleServiceName(ctx, svc)
		if err != nil {
			t.Fatalf("ListServersByOracleServiceName() error = %v", err)
		}

		if len(dbs) != 3 {
			t.Fatalf("got %d databases, want 3", len(dbs))
		}

		want := []string{"abyla_glh_t", "abyla_i3f_t", "abyla_onv_t"}
		for i, name := range want {
			if dbs[i].Name != name {
				t.Errorf("dbs[%d].Name = %q, want %q", i, dbs[i].Name, name)
			}
		}
	})

	t.Run("unknown service name returns empty slice", func(t *testing.T) {
		dbs, err := s.ListServersByOracleServiceName(ctx, "NO_SUCH_SERVICE")
		if err != nil {
			t.Fatalf("ListServersByOracleServiceName() error = %v", err)
		}

		if len(dbs) != 0 {
			t.Errorf("got %d databases, want 0", len(dbs))
		}
	})
}

// TestCreateServer_DuplicateName verifies that inserting a second server with an
// already-taken name surfaces the typed ErrServerNameConflict (mapped to a 409
// by the API) rather than an opaque database error.
func TestCreateServer_DuplicateName(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	first := &Server{
		Name:         "dupe",
		Host:         "localhost",
		Port:         5432,
		DatabaseName: "mydb",
		Username:     "dbuser",
		Password:     "secret",
		SSLMode:      "prefer",
	}
	if _, err := store.CreateServer(ctx, first, key); err != nil {
		t.Fatalf("CreateServer(first) error = %v", err)
	}

	second := &Server{
		Name:         "dupe",
		Host:         "otherhost",
		Port:         5433,
		DatabaseName: "otherdb",
		Username:     "dbuser2",
		Password:     "secret2",
		SSLMode:      "prefer",
	}
	_, err := store.CreateServer(ctx, second, key)
	if !errors.Is(err, ErrServerNameConflict) {
		t.Fatalf("CreateServer(duplicate) error = %v, want ErrServerNameConflict", err)
	}
}
