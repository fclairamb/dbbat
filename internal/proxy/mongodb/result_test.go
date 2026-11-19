package mongodb

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func marshalBody(t *testing.T, doc bson.D) bson.Raw {
	t.Helper()

	raw, err := bson.Marshal(doc)
	require.NoError(t, err)

	return raw
}

// TestBuildSQLTextCutsOnRune pads a command so a multi-byte character straddles
// maxSQLTextLen: a byte cut leaves invalid UTF-8, which the queries insert
// refuses, so the command would never be logged.
func TestBuildSQLTextCutsOnRune(t *testing.T) {
	t.Parallel()

	// Find the padding that puts "é" (2 bytes) across the limit.
	prefix := buildSQLText("find", marshalBody(t, bson.D{{Key: "find", Value: ""}}))
	// prefix is `find {"find": ""}`; the value starts one byte before the
	// closing `"}`.
	valueStart := len(prefix) - 2
	pad := strings.Repeat("a", maxSQLTextLen-valueStart-1)

	text := buildSQLText("find", marshalBody(t, bson.D{{Key: "find", Value: pad + "é" + strings.Repeat("b", 100)}}))

	require.True(t, utf8.ValidString(text), "truncated text must be valid UTF-8")
	require.LessOrEqual(t, len(text), maxSQLTextLen)
	require.Len(t, text, maxSQLTextLen-1, "the cut steps back over the split rune only")
	require.True(t, strings.HasSuffix(text, "a"))
}

func TestBuildSQLTextASCII(t *testing.T) {
	t.Parallel()

	long := buildSQLText("insert", marshalBody(t, bson.D{{Key: "insert", Value: strings.Repeat("x", 2*maxSQLTextLen)}}))
	require.Len(t, long, maxSQLTextLen, "ASCII is cut at exactly the limit")

	short := buildSQLText("find", marshalBody(t, bson.D{{Key: "find", Value: "users"}}))
	require.Equal(t, `find {"find":"users"}`, short, "short text is unchanged")
}
