package mysql

import (
	"errors"
	"testing"

	"github.com/fclairamb/dbbat/internal/store"
)

func TestIsAPIKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"too short", "dbb_", false},
		{"good prefix and length", "dbb_abcdefghijklmnopqrstuvwx12345678", true},
		{"plain password", "secret", false},
		{"web key prefix is not API key", "web_abcdefghijklmnopqrstuvwx12345678", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := isAPIKey(tc.in); got != tc.want {
				t.Errorf("isAPIKey(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestCheckQuotas(t *testing.T) {
	t.Parallel()

	maxQ := int64(10)
	maxB := int64(1000)

	cases := []struct {
		name    string
		grant   *store.Grant
		wantErr error
	}{
		{
			name:    "no quotas configured",
			grant:   &store.Grant{Definition: &store.GrantDefinition{}},
			wantErr: nil,
		},
		{
			name:    "under query quota",
			grant:   &store.Grant{QueryCount: 5, Definition: &store.GrantDefinition{MaxQueryCounts: &maxQ}},
			wantErr: nil,
		},
		{
			name:    "at query quota",
			grant:   &store.Grant{QueryCount: 10, Definition: &store.GrantDefinition{MaxQueryCounts: &maxQ}},
			wantErr: ErrQueryLimitExceeded,
		},
		{
			name:    "over query quota",
			grant:   &store.Grant{QueryCount: 11, Definition: &store.GrantDefinition{MaxQueryCounts: &maxQ}},
			wantErr: ErrQueryLimitExceeded,
		},
		{
			name:    "at byte quota",
			grant:   &store.Grant{BytesTransferred: 1000, Definition: &store.GrantDefinition{MaxBytesTransferred: &maxB}},
			wantErr: ErrDataLimitExceeded,
		},
		{
			name:    "query quota wins over byte quota",
			grant:   &store.Grant{QueryCount: 10, BytesTransferred: 1000, Definition: &store.GrantDefinition{MaxQueryCounts: &maxQ, MaxBytesTransferred: &maxB}},
			wantErr: ErrQueryLimitExceeded,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := checkQuotas(tc.grant)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("checkQuotas() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
