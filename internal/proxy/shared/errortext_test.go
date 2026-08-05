package shared_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
)

func TestSanitizeQueryError(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)

	tests := []struct {
		name  string
		input *string
		// want is the stored text; an empty want with keep=false means the
		// value must be dropped.
		want string
		keep bool
	}{
		{name: "nil stays nil", input: nil},
		{
			name:  "oracle diagnostic",
			input: ptr("ORA-00942: table or view does not exist"),
			want:  "ORA-00942: table or view does not exist", keep: true,
		},
		{
			name:  "mysql diagnostic",
			input: ptr("Error 1146 (42S02): Table 'db.t' doesn't exist"),
			want:  "Error 1146 (42S02): Table 'db.t' doesn't exist", keep: true,
		},
		{
			name:  "mongo diagnostic",
			input: ptr("not authorized on db to execute command"),
			want:  "not authorized on db to execute command", keep: true,
		},
		{
			name:  "multi-line postgres diagnostic",
			input: ptr("ERROR: syntax error\nDETAIL: near \"slect\"\n\tHINT: typo?"),
			want:  "ERROR: syntax error\nDETAIL: near \"slect\"\n\tHINT: typo?", keep: true,
		},
		{
			name:  "utf-8 accents pass through untouched",
			input: ptr("ORA-00001: contrainte unique violée"),
			want:  "ORA-00001: contrainte unique violée", keep: true,
		},
		{
			// The live case this gate must not break: an Oracle session on a
			// single-byte charset (WE8ISO8859P1) emits 0xE9 for "é", which is
			// not valid UTF-8. The diagnostic is kept, mangled but readable.
			name:  "latin-1 diagnostic is repaired, not dropped",
			input: ptr("ORA-00001: contrainte unique viol\xe9e"),
			want:  "ORA-00001: contrainte unique viol�e", keep: true,
		},
		{
			name:  "latin-1 with several accents",
			input: ptr("ORA-12154: r\xe9solution du nom de service impossible (cha\xeene)"),
			want:  "ORA-12154: r�solution du nom de service impossible (cha�ne)", keep: true,
		},
		{name: "empty string", input: ptr(""), want: "", keep: true},
		{name: "control byte inside otherwise valid text", input: ptr("ORA-\xff\xfe\x00 broken")},
		{name: "row bitmask descriptors", input: ptr("\x15\x03\x07\x01\xc2\x0a1-Habitation")},
		{name: "embedded NUL", input: ptr("ORA-00942\x00padding")},
		{name: "DEL byte", input: ptr("ORA-00942\x7f")},
		{name: "C1 control", input: ptr("ORA-00942\u0085trailing")},
		{
			// No control bytes, but overwhelmingly undecodable: binary, not a
			// sentence with accents in it.
			name:  "mostly undecodable binary",
			input: ptr(strings.Repeat("\xe9\xfe\xff\xf5", 40)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := shared.SanitizeQueryError(context.Background(), logger, tt.input)

			if !tt.keep {
				assert.Nil(t, got)

				return
			}

			require.NotNil(t, got)
			assert.Equal(t, tt.want, *got)
		})
	}
}

// TestSanitizeQueryError_NilLogger keeps the helper usable from a code path
// that has no logger to hand.
func TestSanitizeQueryError_NilLogger(t *testing.T) {
	t.Parallel()

	assert.Nil(t, shared.SanitizeQueryError(context.Background(), nil, ptr("bad\x00text")))
}

func ptr(s string) *string {
	return &s
}
