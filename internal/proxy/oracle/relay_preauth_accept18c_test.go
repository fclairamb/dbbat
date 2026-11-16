package oracle

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestStripAcceptModernAuthFlags_18cShortAccept: Oracle 18c XE's Accept is 41
// bytes, too short to carry the connect flags at [41:45] (issue #500). dbbat
// forwards it untouched rather than reading past the end.
func TestStripAcceptModernAuthFlags_18cShortAccept(t *testing.T) {
	t.Parallel()

	raw := make([]byte, 41)
	binary.BigEndian.PutUint16(raw[0:], 41)
	raw[4] = byte(TNSPacketTypeAccept)
	binary.BigEndian.PutUint16(raw[8:], 315)
	binary.BigEndian.PutUint16(raw[20:], 41)

	orig := bytes.Clone(raw)

	assert.False(t, stripAcceptModernAuthFlags(raw))
	assert.Equal(t, orig, raw)
}
