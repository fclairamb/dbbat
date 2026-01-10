package crypto

import (
	"testing"
)

func TestHashPassword(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		password string
	}{
		{name: "simple password", password: "password123"},
		{name: "empty password", password: ""},
		{name: "long password", password: "this is a very long password with special characters !@#$%^&*()"},
		{name: "unicode password", password: "密码123"}, //nolint:gosmopolitan // testing unicode
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			hash, err := HashPassword(tt.password)
			if err != nil {
				t.Fatalf("HashPassword() error = %v", err)
			}

			if hash == "" {
				t.Error("HashPassword() returned empty hash")
			}

			// Hash should start with the argon2id identifier
			if len(hash) < 10 || hash[:9] != "$argon2id" {
				t.Errorf("HashPassword() hash format invalid, got %s", hash)
			}
		})
	}
}

func TestVerifyPassword(t *testing.T) {
	t.Parallel()

	password := "testpassword123"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	tests := []struct {
		name     string
		hash     string
		password string
		want     bool
		wantErr  bool
	}{
		{name: "correct password", hash: hash, password: password, want: true, wantErr: false},
		{name: "wrong password", hash: hash, password: "wrongpassword", want: false, wantErr: false},
		{name: "empty password", hash: hash, password: "", want: false, wantErr: false},
		{name: "invalid hash format", hash: "invalid", password: password, want: false, wantErr: true},
		{name: "empty hash", hash: "", password: password, want: false, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := VerifyPassword(tt.hash, tt.password)
			if (err != nil) != tt.wantErr {
				t.Errorf("VerifyPassword() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("VerifyPassword() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHashPasswordUnique(t *testing.T) {
	t.Parallel()

	password := "samepassword"

	hash1, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	hash2, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	// Same password should produce different hashes (due to random salt)
	if hash1 == hash2 {
		t.Error("HashPassword() produced same hash for same password (salt should be random)")
	}

	// Both hashes should verify against the original password
	valid1, err := VerifyPassword(hash1, password)
	if err != nil || !valid1 {
		t.Error("First hash should verify against original password")
	}

	valid2, err := VerifyPassword(hash2, password)
	if err != nil || !valid2 {
		t.Error("Second hash should verify against original password")
	}
}
