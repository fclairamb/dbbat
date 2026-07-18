package store

import (
	"context"
	"errors"
	"testing"
)

// makeSSHServer creates an SSH bastion row with the given name and secrets.
func makeSSHServer(t *testing.T, s *Store, key []byte, name, privateKey string) *Server {
	t.Helper()
	ctx := context.Background()
	srv := &Server{
		Name:     name,
		Host:     "bastion.example.com",
		Port:     22,
		Username: "www-data",
		Protocol: ProtocolSSH,
		ProtocolData: &ServerProtocolData{
			SSH: &SSHServerData{PrivateKey: privateKey},
		},
	}
	created, err := s.CreateServer(ctx, srv, key)
	if err != nil {
		t.Fatalf("CreateServer(ssh) error = %v", err)
	}
	return created
}

func TestSSHServer_SecretRoundTrip(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	const pk = "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n"
	created := makeSSHServer(t, s, key, "bastion1", pk)

	// The plaintext must not be persisted; reload and decrypt.
	reloaded, err := s.GetServerByUID(ctx, created.UID)
	if err != nil {
		t.Fatalf("GetServerByUID() error = %v", err)
	}
	sd := reloaded.SSHData()
	if sd == nil {
		t.Fatal("SSHData() = nil after reload")
	}
	if len(sd.PrivateKeyEncrypted) == 0 {
		t.Fatal("PrivateKeyEncrypted is empty after reload")
	}
	if sd.PrivateKey != "" {
		t.Errorf("PrivateKey plaintext leaked into storage: %q", sd.PrivateKey)
	}

	if err := reloaded.DecryptSSHSecrets(key); err != nil {
		t.Fatalf("DecryptSSHSecrets() error = %v", err)
	}
	if reloaded.SSHData().PrivateKey != pk {
		t.Errorf("decrypted private key = %q, want %q", reloaded.SSHData().PrivateKey, pk)
	}
}

func TestSSHServer_ExcludedFromTargets(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	bastion := makeSSHServer(t, s, key, "bastion-excl", "pk")

	// Also create a real target so the lists are non-empty.
	if _, err := s.CreateServer(ctx, &Server{
		Name: "realdb", Host: "db", Port: 5432, DatabaseName: "app",
		Username: "u", Password: "p", Protocol: ProtocolPostgreSQL, Listable: true,
	}, key); err != nil {
		t.Fatalf("CreateServer(target) error = %v", err)
	}

	targets, err := s.ListServers(ctx)
	if err != nil {
		t.Fatalf("ListServers() error = %v", err)
	}
	for _, srv := range targets {
		if srv.UID == bastion.UID {
			t.Error("ListServers() returned an ssh bastion row")
		}
	}

	listable, err := s.ListListableServers(ctx)
	if err != nil {
		t.Fatalf("ListListableServers() error = %v", err)
	}
	for _, srv := range listable {
		if srv.UID == bastion.UID {
			t.Error("ListListableServers() returned an ssh bastion row")
		}
	}

	// Even if forced listable, GetServerByName must not resolve an ssh row.
	if _, err := s.GetServerByName(ctx, "bastion-excl"); !errors.Is(err, ErrServerNotFound) {
		t.Errorf("GetServerByName(ssh) error = %v, want ErrServerNotFound", err)
	}

	// But the dedicated SSH listing does return it.
	sshList, err := s.ListSSHServers(ctx)
	if err != nil {
		t.Fatalf("ListSSHServers() error = %v", err)
	}
	found := false
	for _, srv := range sshList {
		if srv.UID == bastion.UID {
			found = true
		}
	}
	if !found {
		t.Error("ListSSHServers() did not return the bastion row")
	}
}

func TestSSHServer_ViaUIDValidation(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	bastion := makeSSHServer(t, s, key, "bastion-via", "pk")

	// A database target may reference the ssh bastion via via_uid.
	target := &Server{
		Name: "tunneled-db", Host: "10.0.0.5", Port: 5432, DatabaseName: "app",
		Username: "u", Password: "p", Protocol: ProtocolPostgreSQL,
		ViaUID: &bastion.UID,
	}
	created, err := s.CreateServer(ctx, target, key)
	if err != nil {
		t.Fatalf("CreateServer(via ssh) error = %v", err)
	}
	if created.ViaUID == nil || *created.ViaUID != bastion.UID {
		t.Fatalf("ViaUID not persisted: %v", created.ViaUID)
	}

	// via_uid must reference an ssh row, not a database target.
	dbTarget, err := s.CreateServer(ctx, &Server{
		Name: "plain-db", Host: "db", Port: 5432, DatabaseName: "app",
		Username: "u", Password: "p", Protocol: ProtocolPostgreSQL,
	}, key)
	if err != nil {
		t.Fatalf("CreateServer(plain) error = %v", err)
	}
	_, err = s.CreateServer(ctx, &Server{
		Name: "bad-via", Host: "h", Port: 5432, DatabaseName: "app",
		Username: "u", Password: "p", Protocol: ProtocolPostgreSQL,
		ViaUID: &dbTarget.UID,
	}, key)
	if !errors.Is(err, ErrServerViaNotSSH) {
		t.Errorf("CreateServer(via non-ssh) error = %v, want ErrServerViaNotSSH", err)
	}

	// A self-referencing update must be rejected as a cycle.
	if err := s.UpdateServer(ctx, bastion.UID, ServerUpdate{ViaUID: &bastion.UID}, key); !errors.Is(err, ErrServerViaCycle) {
		t.Errorf("UpdateServer(self via) error = %v, want ErrServerViaCycle", err)
	}
}

func TestSSHServer_UpdateSecretsAndClearVia(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	bastion := makeSSHServer(t, s, key, "bastion-upd", "old-pk")

	// Update the private key.
	newPK := "new-private-key"
	if err := s.UpdateServer(ctx, bastion.UID, ServerUpdate{SSHPrivateKey: &newPK}, key); err != nil {
		t.Fatalf("UpdateServer(ssh key) error = %v", err)
	}
	reloaded, err := s.GetServerByUID(ctx, bastion.UID)
	if err != nil {
		t.Fatalf("GetServerByUID() error = %v", err)
	}
	if err := reloaded.DecryptSSHSecrets(key); err != nil {
		t.Fatalf("DecryptSSHSecrets() error = %v", err)
	}
	if reloaded.SSHData().PrivateKey != newPK {
		t.Errorf("updated private key = %q, want %q", reloaded.SSHData().PrivateKey, newPK)
	}

	// A target tunneling through the bastion can have its tunnel cleared.
	target, err := s.CreateServer(ctx, &Server{
		Name: "clear-db", Host: "h", Port: 5432, DatabaseName: "app",
		Username: "u", Password: "p", Protocol: ProtocolPostgreSQL, ViaUID: &bastion.UID,
	}, key)
	if err != nil {
		t.Fatalf("CreateServer(via) error = %v", err)
	}
	if err := s.UpdateServer(ctx, target.UID, ServerUpdate{ClearViaUID: true}, key); err != nil {
		t.Fatalf("UpdateServer(clear via) error = %v", err)
	}
	cleared, err := s.GetServerByUID(ctx, target.UID)
	if err != nil {
		t.Fatalf("GetServerByUID() error = %v", err)
	}
	if cleared.ViaUID != nil {
		t.Errorf("ViaUID after clear = %v, want nil", cleared.ViaUID)
	}
}
