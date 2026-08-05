package shared_test

import (
	"context"
	"log/slog"
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
		keep  bool
	}{
		{name: "nil stays nil", input: nil, keep: false},
		{name: "oracle diagnostic", input: ptr("ORA-00942: table or view does not exist"), keep: true},
		{name: "mysql diagnostic", input: ptr("Error 1146 (42S02): Table 'db.t' doesn't exist"), keep: true},
		{name: "mongo diagnostic", input: ptr("not authorized on db to execute command"), keep: true},
		{name: "multi-line postgres diagnostic", input: ptr("ERROR: syntax error\nDETAIL: near \"slect\"\n\tHINT: typo?"), keep: true},
		{name: "accented text", input: ptr("ORA-00001: contrainte unique violée"), keep: true},
		{name: "empty string", input: ptr(""), keep: true},
		{name: "invalid utf-8", input: ptr("ORA-\xff\xfe\x00 broken"), keep: false},
		{name: "row bitmask descriptors", input: ptr("\x15\x03\x07\x01\xc2\x0a1-Habitation"), keep: false},
		{name: "embedded NUL", input: ptr("ORA-00942\x00padding"), keep: false},
		{name: "DEL byte", input: ptr("ORA-00942\x7f"), keep: false},
		{name: "C1 control", input: ptr("ORA-00942\u0085trailing"), keep: false},
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
			assert.Equal(t, *tt.input, *got)
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
