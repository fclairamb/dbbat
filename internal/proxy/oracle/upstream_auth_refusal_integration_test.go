//go:build integration

package oracle

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	go_ora "github.com/sijms/go-ora/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// pythonConnectScript only logs in: the failure under test happens before the
// first statement.
const pythonConnectScript = `
import sys
import oracledb

host, port, service, user, password = sys.argv[1:6]
try:
    oracledb.connect(user=user, password=password,
                     dsn=oracledb.makedsn(host, int(port), service_name=service))
    print("CONNECTED", flush=True)
except oracledb.Error as e:
    print("refused:", str(e).strip().splitlines()[0], flush=True)
`

// breakStoredPassword swaps the entry's stored password for one the upstream
// rejects, which is what an out-of-date catalog entry looks like in production.
func breakStoredPassword(t *testing.T, env *oracleThroughProxy) {
	t.Helper()

	wrong := "not-the-upstream-password"
	require.NoError(t, env.store.UpdateServer(context.Background(), env.dbUID,
		store.ServerUpdate{Password: &wrong}, []byte("0123456789012345678901234567890X")))
}

// TestIntegration_UpstreamAuthRefusalReachesPythonThin: when the upstream
// rejects the stored credentials, python-oracledb thin used to get the socket
// closed under it and report DPY-4011, which reads as a network problem. It
// must get an ORA error that says which credentials were refused.
func TestIntegration_UpstreamAuthRefusalReachesPythonThin(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	if err := exec.Command("python3", "-c", "import oracledb").Run(); err != nil {
		t.Skip("python-oracledb not installed (pip install oracledb)")
	}

	env := startOracleThroughProxy(t, nil)
	breakStoredPassword(t, env)

	script := filepath.Join(t.TempDir(), "connect.py")
	require.NoError(t, os.WriteFile(script, []byte(pythonConnectScript), 0o600))

	ctx, cancel := context.WithTimeout(context.Background(), refusalDeadline)
	defer cancel()

	out, err := exec.CommandContext(ctx, "python3", script,
		env.host, strconv.Itoa(env.port), env.service, env.username, env.apiKey).CombinedOutput()
	require.NoErrorf(t, err, "python-oracledb did not come back from the login:\n%s", out)

	output := string(out)
	assert.Contains(t, output, "refused: ORA-01017", "the refusal must arrive as an ORA error:\n%s", output)
	assert.Contains(t, output, "credentials dbbat stores", "the message must blame the stored credentials:\n%s", output)
	assert.NotContains(t, output, "DPY-4011", "the socket must not just be closed:\n%s", output)
}

// TestIntegration_UpstreamAuthRefusalReachesGoOra is the same check from go-ora.
func TestIntegration_UpstreamAuthRefusalReachesGoOra(t *testing.T) {
	env := startOracleThroughProxy(t, nil)
	breakStoredPassword(t, env)

	client, err := sql.Open("oracle", go_ora.BuildUrl(env.host, env.port, env.service, env.username, env.apiKey, nil))
	require.NoError(t, err)

	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), refusalDeadline)
	defer cancel()

	err = client.PingContext(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ORA-01017")
	assert.Contains(t, err.Error(), "credentials dbbat stores")
}
