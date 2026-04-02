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
