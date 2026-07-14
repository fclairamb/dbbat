package store

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/xdg-go/scram"

	"github.com/fclairamb/dbbat/internal/crypto"
)

// MongoSCRAMIterations is the PBKDF2 iteration count used when deriving a
// MongoDB SCRAM-SHA-256 verifier from a user's password — MongoDB's own default
// for SCRAM-SHA-256.
const MongoSCRAMIterations = 15000

// mongoSCRAMSaltLength is the length in bytes of the random SCRAM salt.
const mongoSCRAMSaltLength = 16

// deriveMongoSCRAM computes the SCRAM-SHA-256 stored credentials (StoredKey and
// ServerKey) from a plaintext password, salt and iteration count, following
// RFC 5802 / RFC 7677. The password is SASLprep-normalized by the scram client,
// matching what MongoDB drivers do — so a proof computed by a driver validates
// against these keys. The username does not enter the derivation.
func deriveMongoSCRAM(password string, salt []byte, iters int) (storedKey, serverKey []byte, err error) {
	client, err := scram.SHA256.NewClient("dbbat", password, "")
	if err != nil {
		return nil, nil, fmt.Errorf("scram client: %w", err)
	}

	creds, err := client.GetStoredCredentialsWithError(scram.KeyFactors{Salt: string(salt), Iters: iters})
	if err != nil {
		return nil, nil, fmt.Errorf("scram stored credentials: %w", err)
	}

	return creds.StoredKey, creds.ServerKey, nil
}

// SetUserMongoVerifier derives and persists a MongoDB SCRAM-SHA-256 verifier for
// the user from their plaintext password, letting them authenticate to the
// MongoDB proxy with the driver-default SCRAM-SHA-256 instead of PLAIN. The
// StoredKey/ServerKey are encrypted at rest (AAD-bound to the user UID); salt
// and iteration count are public. Called on every password set; other protocol
// material in protocol_data is preserved. A row lock serializes concurrent
// protocol_data writers (e.g. Oracle salt generation).
func (s *Store) SetUserMongoVerifier(ctx context.Context, userID uuid.UUID, password string, encryptionKey []byte) error {
	salt := make([]byte, mongoSCRAMSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("generate mongo SCRAM salt: %w", err)
	}

	storedKey, serverKey, err := deriveMongoSCRAM(password, salt, MongoSCRAMIterations)
	if err != nil {
		return err
	}

	aad := crypto.UserAAD(userID.String())

	encStoredKey, err := crypto.Encrypt(storedKey, encryptionKey, aad)
	if err != nil {
		return fmt.Errorf("encrypt mongo SCRAM stored key: %w", err)
	}

	encServerKey, err := crypto.Encrypt(serverKey, encryptionKey, aad)
	if err != nil {
		return fmt.Errorf("encrypt mongo SCRAM server key: %w", err)
	}

	creds := &MongoSCRAMCredentials{
		Salt:       salt,
		Iterations: MongoSCRAMIterations,
		StoredKey:  encStoredKey,
		ServerKey:  encServerKey,
	}

	return s.persistUserMongoVerifier(ctx, userID, creds)
}

// persistUserMongoVerifier writes the verifier into the user's protocol_data
// under a row lock so a concurrent Oracle-salt write on the same row can't
// clobber it (and vice-versa), then merges rather than replaces protocol_data.
func (s *Store) persistUserMongoVerifier(ctx context.Context, userID uuid.UUID, creds *MongoSCRAMCredentials) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	user := new(User)
	if err := tx.NewSelect().Model(user).Where("uid = ?", userID).For("UPDATE").Scan(ctx); err != nil {
		return fmt.Errorf("lock user: %w", err)
	}

	protocolData := user.ProtocolData
	if protocolData == nil {
		protocolData = &UserProtocolData{}
	}

	if protocolData.MongoDB == nil {
		protocolData.MongoDB = &MongoUserData{}
	}

	protocolData.MongoDB.SCRAMSHA256 = creds

	encoded, err := json.Marshal(protocolData)
	if err != nil {
		return fmt.Errorf("encode user protocol data: %w", err)
	}

	if _, err := tx.NewUpdate().
		Model((*User)(nil)).
		Set("protocol_data = ?::jsonb", string(encoded)).
		Set("updated_at = ?", time.Now()).
		Where("uid = ?", userID).
		Exec(ctx); err != nil {
		return fmt.Errorf("persist mongo SCRAM verifier: %w", err)
	}

	return tx.Commit()
}
