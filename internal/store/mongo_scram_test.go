package store

import (
	"testing"

	"github.com/xdg-go/scram"
)

// TestDeriveMongoSCRAMValidatesClientProof is the core correctness guarantee for
// stored SCRAM verifiers: credentials derived from a password by
// deriveMongoSCRAM must validate a SCRAM-SHA-256 proof computed by a standard
// client using that same password. It runs a full client↔server exchange with a
// credential lookup backed by the derived StoredKey/ServerKey.
func TestDeriveMongoSCRAMValidatesClientProof(t *testing.T) {
	t.Parallel()

	const (
		username = "dbbattest"
		password = "s3cr3t-p@ss"
	)

	salt := []byte("0123456789abcdef")

	storedKey, serverKey, err := deriveMongoSCRAM(password, salt, MongoSCRAMIterations)
	if err != nil {
		t.Fatalf("deriveMongoSCRAM: %v", err)
	}

	lookup := func(string) (scram.StoredCredentials, error) {
		return scram.StoredCredentials{
			KeyFactors: scram.KeyFactors{Salt: string(salt), Iters: MongoSCRAMIterations},
			StoredKey:  storedKey,
			ServerKey:  serverKey,
		}, nil
	}

	server, err := scram.SHA256.NewServer(lookup)
	if err != nil {
		t.Fatalf("new scram server: %v", err)
	}

	client, err := scram.SHA256.NewClient(username, password, "")
	if err != nil {
		t.Fatalf("new scram client: %v", err)
	}

	sConv := server.NewConversation()
	cConv := client.NewConversation()

	// client-first → server-first → client-final → server-final.
	clientFirst, err := cConv.Step("")
	if err != nil {
		t.Fatalf("client-first: %v", err)
	}

	serverFirst, err := sConv.Step(clientFirst)
	if err != nil {
		t.Fatalf("server-first: %v", err)
	}

	clientFinal, err := cConv.Step(serverFirst)
	if err != nil {
		t.Fatalf("client-final: %v", err)
	}

	serverFinal, err := sConv.Step(clientFinal)
	if err != nil {
		t.Fatalf("server-final: %v", err)
	}

	if !sConv.Valid() {
		t.Fatal("server conversation should be valid after a correct proof")
	}

	// The client validates the server signature too — a full mutual auth.
	if _, err := cConv.Step(serverFinal); err != nil {
		t.Fatalf("client validation of server-final: %v", err)
	}

	if !cConv.Valid() {
		t.Fatal("client conversation should be valid after server signature")
	}
}

// TestDeriveMongoSCRAMRejectsWrongPassword confirms a proof from a different
// password does not validate against the derived credentials.
func TestDeriveMongoSCRAMRejectsWrongPassword(t *testing.T) {
	t.Parallel()

	salt := []byte("0123456789abcdef")

	storedKey, serverKey, err := deriveMongoSCRAM("correct-password", salt, MongoSCRAMIterations)
	if err != nil {
		t.Fatalf("deriveMongoSCRAM: %v", err)
	}

	lookup := func(string) (scram.StoredCredentials, error) {
		return scram.StoredCredentials{
			KeyFactors: scram.KeyFactors{Salt: string(salt), Iters: MongoSCRAMIterations},
			StoredKey:  storedKey,
			ServerKey:  serverKey,
		}, nil
	}

	server, _ := scram.SHA256.NewServer(lookup)
	client, _ := scram.SHA256.NewClient("dbbattest", "WRONG-password", "")

	sConv := server.NewConversation()
	cConv := client.NewConversation()

	clientFirst, _ := cConv.Step("")

	serverFirst, err := sConv.Step(clientFirst)
	if err != nil {
		t.Fatalf("server-first: %v", err)
	}

	clientFinal, _ := cConv.Step(serverFirst)

	if _, err := sConv.Step(clientFinal); err == nil && sConv.Valid() {
		t.Fatal("a wrong-password proof must not validate")
	}
}
