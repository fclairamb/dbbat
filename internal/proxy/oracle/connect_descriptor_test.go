package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseServiceName_Standard(t *testing.T) {
	t.Parallel()
	desc := `(DESCRIPTION=(ADDRESS=(PROTOCOL=TCP)(HOST=db.example.com)(PORT=1521))(CONNECT_DATA=(SERVICE_NAME=ORCL)))`
	assert.Equal(t, "ORCL", parseServiceName(desc))
}

func TestParseServiceName_CaseInsensitive(t *testing.T) {
	t.Parallel()
	desc := `(DESCRIPTION=(ADDRESS=(PROTOCOL=TCP)(HOST=db)(PORT=1521))(CONNECT_DATA=(service_name=mydb)))`
	assert.Equal(t, "mydb", parseServiceName(desc))
}

func TestParseServiceName_WithSpaces(t *testing.T) {
	t.Parallel()
	desc := `(DESCRIPTION = (CONNECT_DATA = (SERVICE_NAME = PROD_DB )))`
	assert.Equal(t, "PROD_DB", parseServiceName(desc))
}

func TestParseServiceName_MissingServiceName(t *testing.T) {
	t.Parallel()
	desc := `(DESCRIPTION=(ADDRESS=(PROTOCOL=TCP)(HOST=db)(PORT=1521))(CONNECT_DATA=(SID=ORCL)))`
	assert.Empty(t, parseServiceName(desc))
}

func TestParseSID_Fallback(t *testing.T) {
	t.Parallel()
	desc := `(DESCRIPTION=(CONNECT_DATA=(SID=MYDB)))`
	assert.Empty(t, parseServiceName(desc))
	assert.Equal(t, "MYDB", parseSID(desc))
}

func TestParseServiceName_MultipleAddresses(t *testing.T) {
	t.Parallel()
	desc := `(DESCRIPTION=
        (ADDRESS_LIST=
            (ADDRESS=(PROTOCOL=TCP)(HOST=rac1)(PORT=1521))
            (ADDRESS=(PROTOCOL=TCP)(HOST=rac2)(PORT=1521)))
        (CONNECT_DATA=(SERVICE_NAME=RAC_SVC)(FAILOVER_MODE=(TYPE=SELECT)(METHOD=BASIC))))`
	assert.Equal(t, "RAC_SVC", parseServiceName(desc))
}

func TestParseServiceName_EZConnect(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "ORCL", parseServiceNameEZConnect("db.example.com:1521/ORCL"))
}

func TestParseServiceName_EZConnect_NoPort(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "ORCL", parseServiceNameEZConnect("db.example.com/ORCL"))
}

func TestParseServiceName_EZConnect_NoSlash(t *testing.T) {
	t.Parallel()
	assert.Empty(t, parseServiceNameEZConnect("db.example.com:1521"))
}

func TestParseConnectDescriptor_Full(t *testing.T) {
	t.Parallel()
	desc := `(DESCRIPTION=(ADDRESS=(PROTOCOL=TCP)(HOST=db.prod)(PORT=1521))(CONNECT_DATA=(SERVICE_NAME=FINDB)(CID=(PROGRAM=sqlplus)(HOST=workstation)(USER=jdoe))))`
	cd := parseConnectDescriptor(desc)
	assert.Equal(t, "FINDB", cd.ServiceName)
	assert.Equal(t, "db.prod", cd.Host)
	assert.Equal(t, 1521, cd.Port)
	assert.Equal(t, "sqlplus", cd.Program)
	assert.Equal(t, "jdoe", cd.OSUser)
}

func TestParseConnectDescriptor_Empty(t *testing.T) {
	t.Parallel()
	cd := parseConnectDescriptor("")
	assert.Empty(t, cd.ServiceName)
}

func TestParseConnectDescriptor_MalformedParens(t *testing.T) {
	t.Parallel()
	desc := `(DESCRIPTION=(CONNECT_DATA=(SERVICE_NAME=OK)`
	cd := parseConnectDescriptor(desc)
	assert.Equal(t, "OK", cd.ServiceName)
}

func TestExtractConnectString_WithParen(t *testing.T) {
	t.Parallel()
	// Simulate a payload that just contains a descriptor
	payload := []byte("garbage(DESCRIPTION=(CONNECT_DATA=(SERVICE_NAME=TEST)))")
	s := findDescriptorInPayload(payload)
	assert.Contains(t, s, "SERVICE_NAME=TEST")
}

func TestExtractConnectString_Empty(t *testing.T) {
	t.Parallel()
	s := findDescriptorInPayload([]byte{})
	assert.Empty(t, s)
}

func TestRewriteServiceName(t *testing.T) {
	t.Parallel()

	desc := `(DESCRIPTION=(CONNECT_DATA=(SERVICE_NAME=abynonprod))(ADDRESS=(PROTOCOL=TCP)(HOST=localhost)(PORT=1522)))`
	// Build a minimal payload with the descriptor at offset 26
	payload := make([]byte, 26+len(desc))
	payload[16] = byte(len(desc) >> 8) // connect data length
	payload[17] = byte(len(desc))
	payload[18] = 0      // connect data offset (high)
	payload[19] = 26 + 8 // offset from packet start (payload offset 26 + 8-byte TNS header = 34)
	copy(payload[26:], desc)

	pkt := &TNSPacket{Type: TNSPacketTypeConnect, Payload: payload}

	rewritten := rewriteServiceName(pkt, "abynonprod", "TEST01")

	newDesc := string(rewritten.Payload[26:])
	assert.Contains(t, newDesc, "SERVICE_NAME=TEST01")
	assert.NotContains(t, newDesc, "abynonprod")
	// Raw should be nil so writeTNSPacket re-encodes
	assert.Nil(t, rewritten.Raw)
}

func TestRewriteServiceName_NoChange(t *testing.T) {
	t.Parallel()

	pkt := &TNSPacket{
		Type:    TNSPacketTypeConnect,
		Payload: []byte("(DESCRIPTION=(CONNECT_DATA=(SERVICE_NAME=TEST01)))"),
	}

	// Same name — should return original packet
	result := rewriteServiceName(pkt, "TEST01", "TEST01")
	assert.Equal(t, pkt, result)
}

func TestRewriteServiceName_CaseInsensitive(t *testing.T) {
	t.Parallel()

	pkt := &TNSPacket{
		Type:    TNSPacketTypeConnect,
		Payload: []byte("(DESCRIPTION=(CONNECT_DATA=(service_name=mydb)))"),
	}

	result := rewriteServiceName(pkt, "mydb", "PROD")
	assert.Contains(t, string(result.Payload), "PROD")
	assert.NotContains(t, string(result.Payload), "mydb")
}

func TestRewriteServiceName_PadsShorterName(t *testing.T) {
	t.Parallel()

	pkt := &TNSPacket{
		Type:    TNSPacketTypeConnect,
		Payload: []byte("(DESCRIPTION=(CONNECT_DATA=(SERVICE_NAME=abynonprod)))"),
		Raw:     []byte("HEADER(DESCRIPTION=(CONNECT_DATA=(SERVICE_NAME=abynonprod)))"),
	}

	result := rewriteServiceName(pkt, "abynonprod", "TEST01")
	// TEST01 (6 chars) padded to 10 chars to match abynonprod length
	assert.Contains(t, string(result.Payload), "TEST01")
	assert.NotContains(t, string(result.Payload), "abynonprod")
	// Same length preserved
	assert.Len(t, result.Payload, len(pkt.Payload))
	assert.Len(t, result.Raw, len(pkt.Raw))
}
