//go:build integration

// This suite is what the in-process fake API server
// (internal/proxy/upstream/kubernetes_fake_test.go) structurally cannot cover:
// the kubelet, the websocket-first transport a modern API server negotiates,
// RBAC as the API server really evaluates it, a pod that is destroyed and
// recreated mid-suite, and a real database session dialed through
// `pods/portforward`.
//
// One k3s cluster is shared by the whole package (booting one costs a couple of
// minutes); each test still gets its own dbbat store container, so no test can
// see another's rows.

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/proxy/conncheck"
	"github.com/fclairamb/dbbat/internal/proxy/postgresql"
	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
	"github.com/fclairamb/dbbat/internal/proxy/upstream"
	"github.com/fclairamb/dbbat/internal/store"
)

// storageImage backs dbbat's own store, on the host. Deliberately the same
// image as the in-cluster upstream but a separate constant: a matrix run
// against another in-cluster PostgreSQL must not also swap the storage engine
// underneath dbbat.
const storageImage = "postgres:15-alpine"

const (
	fixtureUser = "dbbattest"
	fixturePass = "dbbattest"
)

// sharedCluster is the one k3s cluster the suite runs against, owned by
// TestMain.
var sharedCluster *k3sCluster //nolint:gochecknoglobals // suite-wide fixture

func TestMain(m *testing.M) {
	ctx := context.Background()

	cluster, err := startK3s(ctx)
	if err != nil {
		cluster.terminate(context.Background())
		fmt.Fprintf(os.Stderr, "kubernetes integration suite: could not start the cluster: %v\n", err)
		os.Exit(1)
	}

	sharedCluster = cluster

	code := m.Run()

	cluster.terminate(context.Background())
	os.Exit(code)
}

// ---------- fixtures ----------

// fixtureOpts tweaks what newFixture builds.
type fixtureOpts struct {
	// serviceAccount names which of the three ServiceAccounts the cluster row
	// authenticates as. Empty means saPortForwardCreate: the narrowest Role
	// that still carries a dial on every supported API server version, since
	// SPDY is the transport all of them speak.
	serviceAccount string
	// host is the database row's host field: a bare pod name, or `svc/<name>`.
	// Empty means `svc/dbbat-pg`.
	host string
	// omitCA leaves the cluster row's CA bundle blank, so the first connect
	// takes the trust-on-first-use pin instead.
	omitCA bool
}

// fixture is a dbbat instance — its own store, a user, a cluster row, a
// database row behind it, a grant and a started PostgreSQL proxy — pointed at
// the shared k3s cluster.
type fixture struct {
	t          *testing.T
	store      *store.Store
	encKey     []byte
	user       *store.User
	clusterUID uuid.UUID
	dbUID      uuid.UUID
	proxy      *postgresql.Server
	proxyAddr  string
}

// runStorageContainer starts the PostgreSQL container backing dbbat's own
// store.
func runStorageContainer(ctx context.Context, t *testing.T) (string, int) {
	t.Helper()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        storageImage,
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_DB":       "dbbat_test",
				"POSTGRES_USER":     upstreamUser,
				"POSTGRES_PASSWORD": upstreamPass,
			},
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(120 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err, "start the dbbat storage container")

	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	require.NoError(t, err)

	port, err := container.MappedPort(ctx, "5432")
	require.NoError(t, err)

	return host, int(port.Num())
}

// newFixture wires a complete dbbat up against the shared cluster.
func newFixture(ctx context.Context, t *testing.T, opts fixtureOpts) *fixture {
	t.Helper()

	sa := opts.serviceAccount
	if sa == "" {
		sa = saPortForwardCreate
	}

	host := opts.host
	if host == "" {
		host = "svc/" + pgService
	}

	storeHost, storePort := runStorageContainer(ctx, t)

	dsn := fmt.Sprintf("postgres://%s:%s@%s/dbbat_test?sslmode=disable",
		upstreamUser, upstreamPass, net.JoinHostPort(storeHost, strconv.Itoa(storePort)))

	encKey := make([]byte, 32)
	for i := range encKey {
		encKey[i] = byte(i + 1)
	}

	dataStore, err := store.New(ctx, dsn, store.Options{EncryptionKey: encKey})
	require.NoError(t, err)
	t.Cleanup(dataStore.Close)
	require.NoError(t, dataStore.Migrate(ctx))

	hash, err := crypto.HashPassword(fixturePass)
	require.NoError(t, err)

	user, err := dataStore.CreateUser(ctx, fixtureUser, hash, []string{"connector"})
	require.NoError(t, err)

	creds := sharedCluster.serviceAccountToken(ctx, t, sa)

	k8sData := &store.KubernetesServerData{Namespace: testNamespace}
	if !opts.omitCA {
		k8sData.CACert = creds.caPEM
	}

	cluster, err := dataStore.CreateServer(ctx, &store.Server{
		Name:     "k3s",
		Host:     sharedCluster.apiHost,
		Port:     sharedCluster.apiPort,
		Username: sa,
		Password: creds.token,
		Protocol: store.ProtocolKubernetes,
		// A cluster row is a dial path, never a grantable target.
		Listable:     false,
		ProtocolData: &store.ServerProtocolData{Kubernetes: k8sData},
	}, encKey)
	require.NoError(t, err)

	// The row's *name* is what a client asks for in its startup message, so it
	// has to be the database name the DSN carries. Each test owns its own store
	// container, so a fixed name collides with nothing.
	db, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         upstreamDB,
		Host:         host,
		Port:         pgContainerPort,
		DatabaseName: upstreamDB,
		Username:     upstreamUser,
		Password:     upstreamPass,
		Protocol:     store.ProtocolPostgreSQL,
		SSLMode:      "disable",
		ViaUID:       &cluster.UID,
		Listable:     true,
	}, encKey)
	require.NoError(t, err)

	_, err = testsupport.CreateGrantWithControls(ctx, t, dataStore, user.UID, db.UID, []string{})
	require.NoError(t, err)

	proxy, err := postgresql.NewServer(dataStore, encKey, config.QueryStorageConfig{
		StoreResults:   true,
		MaxResultRows:  1000,
		MaxResultBytes: 1 * 1024 * 1024,
	}, config.DumpConfig{}, nil, config.PGConfig{}, slog.Default())
	require.NoError(t, err)

	go func() { _ = proxy.Start("127.0.0.1:0") }()

	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proxy.Shutdown(shutdownCtx)
	})

	require.Eventually(t, func() bool { return proxy.Addr() != nil },
		10*time.Second, 50*time.Millisecond, "the proxy never started listening")

	return &fixture{
		t:          t,
		store:      dataStore,
		encKey:     encKey,
		user:       user,
		clusterUID: cluster.UID,
		dbUID:      db.UID,
		proxy:      proxy,
		proxyAddr:  proxy.Addr().String(),
	}
}

// connect dials the dbbat proxy as the fixture user.
func (f *fixture) connect(ctx context.Context) (*pgx.Conn, error) {
	dsn := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
		fixtureUser, fixturePass, f.proxyAddr, upstreamDB)

	return pgx.Connect(ctx, dsn)
}

// server reloads a row from the store.
func (f *fixture) server(ctx context.Context, uid uuid.UUID) *store.Server {
	f.t.Helper()

	srv, err := f.store.GetServerByUID(ctx, uid)
	require.NoError(f.t, err)

	return srv
}

// tunnelFor builds a tunnel directly from a ServiceAccount's credentials,
// bypassing the store. It is how the transport and dial assertions address
// upstream.KubernetesTunnel without a database row in the way.
//
// DialContext is deliberately left nil: that is what selects the
// websocket-first dialer with its SPDY fallback (setting it forces SPDY, for
// the SSH-bastion case).
func tunnelFor(ctx context.Context, t *testing.T, sa string) *upstream.KubernetesTunnel {
	t.Helper()

	creds := sharedCluster.serviceAccountToken(ctx, t, sa)

	tunnel, err := upstream.NewKubernetesTunnel(upstream.KubernetesConfig{
		Host:      sharedCluster.apiHost,
		Port:      sharedCluster.apiPort,
		Token:     creds.token,
		CACert:    creds.caPEM,
		Namespace: testNamespace,
		UserAgent: "dbbat/integration-test",
	})
	require.NoError(t, err)

	return tunnel
}

// pgOverConn speaks PostgreSQL down an already-open tunnel conn. The DSN's host
// is a literal IP so pgx never tries to resolve it — the dial is entirely ours.
func pgOverConn(ctx context.Context, t *testing.T, dial func(context.Context) (net.Conn, error)) *pgx.Conn {
	t.Helper()

	cfg, err := pgx.ParseConfig(fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable",
		upstreamUser, upstreamPass, pgContainerPort, upstreamDB))
	require.NoError(t, err)

	cfg.DialFunc = func(dialCtx context.Context, _, _ string) (net.Conn, error) {
		return dial(dialCtx)
	}

	conn, err := pgx.ConnectConfig(ctx, cfg)
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	return conn
}

// ---------- 1. DialPod and Dial("svc/…") both reach the pod ----------

// TestIntegration_K8s_DialPodAndService is the spec's first assertion: both
// addressing modes a database row may use reach the PostgreSQL pod, and what
// comes back down the stream is really PostgreSQL — a full startup handshake
// and a query, not a byte count.
func TestIntegration_K8s_DialPodAndService(t *testing.T) {
	ctx := context.Background()
	tunnel := tunnelFor(ctx, t, saPortForwardCreate)
	pod := sharedCluster.currentPod(ctx, t)

	t.Run("DialPod", func(t *testing.T) {
		conn := pgOverConn(ctx, t, func(dialCtx context.Context) (net.Conn, error) {
			return tunnel.DialPod(dialCtx, pod, pgContainerPort)
		})

		var got int
		require.NoError(t, conn.QueryRow(ctx, "SELECT 1").Scan(&got))
		assert.Equal(t, 1, got)
	})

	t.Run("Dial_service", func(t *testing.T) {
		// The service resolves through EndpointSlices to a *ready* endpoint,
		// which is the whole reason `svc/<name>` survives a rescheduled pod.
		resolved, err := tunnel.ResolvePod(ctx, "svc/"+pgService)
		require.NoError(t, err)
		assert.Equal(t, pod, resolved, "svc/ must resolve to the ready pod behind it")

		conn := pgOverConn(ctx, t, func(dialCtx context.Context) (net.Conn, error) {
			return tunnel.Dial(dialCtx, "svc/"+pgService, pgContainerPort)
		})

		var got string
		require.NoError(t, conn.QueryRow(ctx, "SELECT current_database()").Scan(&got))
		assert.Equal(t, upstreamDB, got)
	})

	t.Run("pod_readiness_and_rbac_are_answerable", func(t *testing.T) {
		require.NoError(t, tunnel.CheckPodReady(ctx, pod))

		allowed, _, err := tunnel.PortForwardAllowed(ctx)
		require.NoError(t, err)
		assert.True(t, allowed, "the documented Role must satisfy the SelfSubjectAccessReview")
	})

	t.Run("missing_pod_is_told_apart", func(t *testing.T) {
		_, err := tunnel.Dial(ctx, "no-such-pod", pgContainerPort)
		require.Error(t, err)
		assert.True(t, errors.Is(err, upstream.ErrKubernetesTargetNotFound) ||
			errors.Is(err, upstream.ErrKubernetesForbidden),
			"a missing pod must classify as a cluster-side failure, got: %v", err)
	})
}

// ---------- the transport the fake cannot serve ----------

// TestIntegration_K8s_WebsocketTransport proves the tunnel really negotiates
// the **websocket** transport against a modern API server, and really falls
// back to SPDY when it cannot.
//
// The proof is RBAC, not instrumentation. client-go's fallback dialer reports
// nothing about which of its two dialers won, so instead the two dials below
// are made with ServiceAccounts whose Roles admit exactly one transport each:
//
//   - the API server derives the RBAC verb from the HTTP method (GET → `get`,
//     POST → `create`);
//   - the websocket upgrade is a GET, the SPDY upgrade is a POST.
//
// So saPortForwardGet can *only* succeed over websockets, and
// saPortForwardCreate can *only* succeed after the websocket GET is refused
// (403 → an httpstream upgrade failure → the fallback fires) and the SPDY POST
// takes over. Together they pin down both halves of the transport choice.
//
// Neither fixture Role is the one an operator should copy: docs/kubernetes.md
// grants `create` **and** `get` so that neither transport is ever refused. The
// fixture splits that Role in half on purpose — a Role granting both would make
// every dial ambiguous and prove nothing about which transport carried it.
//
// What the create-only half demonstrates in passing is the cost of the advice
// that predates this suite: a deployment granting `create` alone still works,
// but pays a rejected websocket GET on every single dial before the fallback
// fires.
func TestIntegration_K8s_WebsocketTransport(t *testing.T) {
	ctx := context.Background()
	pod := sharedCluster.currentPod(ctx, t)

	t.Run("websocket_only_role_can_dial", func(t *testing.T) {
		tunnel := tunnelFor(ctx, t, saPortForwardGet)

		conn := pgOverConn(ctx, t, func(dialCtx context.Context) (net.Conn, error) {
			return tunnel.DialPod(dialCtx, pod, pgContainerPort)
		})

		var got int
		require.NoError(t, conn.QueryRow(ctx, "SELECT 1").Scan(&got),
			"a Role carrying only `get` on pods/portforward can only be satisfied by the websocket upgrade")
		assert.Equal(t, 1, got)

		// The SelfSubjectAccessReview asks about `create`, so this Role reads
		// as "not allowed" even though the websocket dial above works. That is
		// a real gap in the connectivity check, not an accident of the fixture:
		// see specs/todos/2026-08-11-05-conncheck-portforward-rbac-both-verbs.md.
		// This assertion is what will have to flip when that lands.
		allowed, _, err := tunnel.PortForwardAllowed(ctx)
		require.NoError(t, err)
		assert.False(t, allowed,
			"PortForwardAllowed only asks about `create`; see the follow-up todo")
	})

	t.Run("spdy_fallback_carries_a_create_only_role", func(t *testing.T) {
		tunnel := tunnelFor(ctx, t, saPortForwardCreate)

		conn := pgOverConn(ctx, t, func(dialCtx context.Context) (net.Conn, error) {
			return tunnel.DialPod(dialCtx, pod, pgContainerPort)
		})

		var got int
		require.NoError(t, conn.QueryRow(ctx, "SELECT 1").Scan(&got),
			"a Role carrying only `create` forces the websocket GET to 403 and the SPDY fallback to carry the dial")
		assert.Equal(t, 1, got)
	})
}

// ---------- 2. a full dbbat proxy session over the tunnel ----------

// TestIntegration_K8s_ProxySessionLogsQueries is the spec's second assertion: a
// client connects to dbbat, dbbat reaches the database through
// `pods/portforward`, and the whole ordinary pipeline — auth, grant, query
// logging, ssl_mode *inside* the tunnel — runs unchanged.
func TestIntegration_K8s_ProxySessionLogsQueries(t *testing.T) {
	ctx := context.Background()
	f := newFixture(ctx, t, fixtureOpts{})

	conn, err := f.connect(ctx)
	require.NoError(t, err, "a client session through the tunnel must complete")

	defer func() { _ = conn.Close(context.Background()) }()

	marker := "k8s_tunnel_" + uuid.NewString()[:8]

	_, err = conn.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+marker+" (id int)")
	require.NoError(t, err)

	_, err = conn.Exec(ctx, "INSERT INTO "+marker+" (id) VALUES (42)")
	require.NoError(t, err)

	var got int
	require.NoError(t, conn.QueryRow(ctx, "SELECT id FROM "+marker).Scan(&got))
	assert.Equal(t, 42, got)

	// The connection row proves the session was authenticated and stamped like
	// any other, tunnel or not.
	connections, err := f.store.ListConnections(ctx, store.ConnectionFilter{UserID: &f.user.UID})
	require.NoError(t, err)
	require.NotEmpty(t, connections, "the handshake must have created a connection row")
	assert.Equal(t, f.dbUID, connections[0].DatabaseID)

	// Query logging is written off the session's own goroutine, so the last
	// statement's row can land a moment after its result has reached the client
	// — poll rather than read once. (Nothing tunnel-specific: it is why this is
	// an Eventually and not an assertion on a single snapshot.)
	logged := func() (create, insert, sel bool) {
		queries, err := f.store.ListQueries(ctx, store.QueryFilter{UserID: &f.user.UID, Limit: 100})
		if err != nil {
			return false, false, false
		}

		for _, q := range queries {
			switch {
			case strings.Contains(q.SQLText, "CREATE TABLE") && strings.Contains(q.SQLText, marker):
				create = true
			case strings.Contains(q.SQLText, "INSERT INTO") && strings.Contains(q.SQLText, marker):
				insert = true
			case strings.Contains(q.SQLText, "SELECT id FROM "+marker):
				sel = true
			}
		}

		return create, insert, sel
	}

	assert.Eventually(t, func() bool {
		create, insert, sel := logged()

		return create && insert && sel
	}, 20*time.Second, 250*time.Millisecond, "the tunnel session's statements never all reached the query log")

	// Named individually so a failure says *which* statement went missing.
	sawCreate, sawInsert, sawSelect := logged()
	assert.True(t, sawCreate, "the CREATE went unlogged")
	assert.True(t, sawInsert, "the INSERT went unlogged")
	assert.True(t, sawSelect, "the SELECT went unlogged")
}

// ---------- 3. the pod is destroyed and recreated ----------

// TestIntegration_K8s_PodRestartRecovers is the spec's third assertion. A
// port-forward is a chain that terminates in one pod: killing that pod kills
// every stream into it, and only a `svc/<name>` row can re-resolve. Both
// halves are asserted — the in-flight session dies, the next connection
// succeeds against the *new* pod.
func TestIntegration_K8s_PodRestartRecovers(t *testing.T) {
	ctx := context.Background()
	f := newFixture(ctx, t, fixtureOpts{})

	before := sharedCluster.currentPod(ctx, t)

	live, err := f.connect(ctx)
	require.NoError(t, err)

	defer func() { _ = live.Close(context.Background()) }()

	var got int
	require.NoError(t, live.QueryRow(ctx, "SELECT 1").Scan(&got))
	require.Equal(t, 1, got)

	sharedCluster.kubectl(ctx, t, "-n", testNamespace, "delete", "pod", before, "--wait=false")

	after, err := sharedCluster.awaitReadyPod(ctx, before, 10*time.Minute)
	require.NoError(t, err, "the Deployment must recreate the pod")
	require.NotEqual(t, before, after)

	t.Logf("pod %s replaced by %s", before, after)

	// The old session died with its pod. pgx may notice on the very next
	// round trip or on the one after (the stream close races the query), so
	// give it a bounded number of attempts rather than a single shot.
	require.Eventually(t, func() bool {
		var v int

		return live.QueryRow(ctx, "SELECT 1").Scan(&v) != nil
	}, 30*time.Second, time.Second, "the session into the deleted pod must not survive it")

	// And the next one re-resolves svc/ to the new pod and works.
	fresh, err := f.connect(ctx)
	require.NoError(t, err, "the next connection must re-resolve svc/ to the recreated pod")

	defer func() { _ = fresh.Close(context.Background()) }()

	require.NoError(t, fresh.QueryRow(ctx, "SELECT 1").Scan(&got))
	assert.Equal(t, 1, got)

	// A row addressing the *old* pod by name is the case that does not
	// recover — worth pinning down, because it is the difference between the
	// two addressing modes.
	stale := tunnelFor(ctx, t, saPortForwardCreate)
	_, err = stale.Dial(ctx, before, pgContainerPort)
	require.Error(t, err, "a bare pod name must not survive the pod")
	assert.True(t, errors.Is(err, upstream.ErrKubernetesTargetNotFound) ||
		errors.Is(err, upstream.ErrKubernetesForbidden),
		"a vanished pod must classify as a cluster-side failure, got: %v", err)
}

// ---------- 4. a Role without pods/portforward ----------

// TestIntegration_K8s_MissingRBACIsNamed is the spec's fourth assertion: a
// ServiceAccount whose Role carries no `pods/portforward` verb must be reported
// as an RBAC problem by the connectivity check — `cluster_rbac` /
// `k8s_forbidden` — and never as the database refusing us.
func TestIntegration_K8s_MissingRBACIsNamed(t *testing.T) {
	ctx := context.Background()
	f := newFixture(ctx, t, fixtureOpts{serviceAccount: saNoPortForward})

	checker := conncheck.New(f.store, f.encKey).WithTimeout(60 * time.Second)

	t.Run("cluster_row", func(t *testing.T) {
		res := checker.Check(ctx, f.server(ctx, f.clusterUID))

		assert.False(t, res.OK)
		assert.Equal(t, conncheck.StageClusterRBAC, res.Stage)
		assert.Equal(t, conncheck.CodeK8sForbidden, res.Code)
		assert.Contains(t, res.Message, "pods/portforward")
	})

	t.Run("database_row_behind_it", func(t *testing.T) {
		// Testing the *database* row runs the same cluster pre-flight before
		// it dials, so a missing verb is never reported as a database error.
		res := checker.Check(ctx, f.server(ctx, f.dbUID))

		assert.False(t, res.OK)
		assert.Equal(t, conncheck.StageClusterRBAC, res.Stage)
		assert.Equal(t, conncheck.CodeK8sForbidden, res.Code)
		assert.NotEqual(t, conncheck.CodeDBAuthFailed, res.Code)
	})

	t.Run("a_real_dial_is_forbidden_not_a_database_error", func(t *testing.T) {
		tunnel := tunnelFor(ctx, t, saNoPortForward)
		pod := sharedCluster.currentPod(ctx, t)

		_, err := tunnel.DialPod(ctx, pod, pgContainerPort)
		require.Error(t, err)
		assert.ErrorIs(t, err, upstream.ErrKubernetesForbidden)
	})

	t.Run("a_client_session_fails_before_the_database", func(t *testing.T) {
		_, err := f.connect(ctx)
		require.Error(t, err, "no grant can make up for a missing RBAC verb")
	})
}

// ---------- the happy path of the check, and the TOFU pin ----------

// TestIntegration_K8s_ConncheckSucceeds is the counterpart of the RBAC failure
// above: the documented Role walks the whole ladder — API server reachable,
// token accepted, port-forward permitted, pod resolved and Ready.
func TestIntegration_K8s_ConncheckSucceeds(t *testing.T) {
	ctx := context.Background()
	f := newFixture(ctx, t, fixtureOpts{})

	checker := conncheck.New(f.store, f.encKey).WithTimeout(60 * time.Second)

	cluster := checker.Check(ctx, f.server(ctx, f.clusterUID))
	assert.True(t, cluster.OK, "cluster check failed: %s", cluster.Message)
	assert.Equal(t, conncheck.StageClusterRBAC, cluster.Stage)
	assert.False(t, cluster.CAPinned, "a row that supplied its own bundle must not pin one")

	target := checker.Check(ctx, f.server(ctx, f.dbUID))
	assert.True(t, target.OK, "database check failed: %s", target.Message)
}

// TestIntegration_K8s_TOFUPinsTheRealAPIServerCA covers the trust-on-first-use
// capture that spec 2026-08-10-15 added, against a real API server rather than
// a fake: a cluster row supplying no CA bundle learns the one the API server
// actually presents, records it, and verifies against it afterwards.
func TestIntegration_K8s_TOFUPinsTheRealAPIServerCA(t *testing.T) {
	ctx := context.Background()
	f := newFixture(ctx, t, fixtureOpts{omitCA: true})

	require.Empty(t, f.server(ctx, f.clusterUID).KubernetesData().LearnedCACert,
		"nothing may be pinned before the first connect")

	checker := conncheck.New(f.store, f.encKey).WithTimeout(60 * time.Second)

	first := checker.Check(ctx, f.server(ctx, f.clusterUID))
	require.True(t, first.OK, "the first connect failed: %s", first.Message)
	assert.True(t, first.CAPinned, "the first connect must report the pin it performed")
	assert.Contains(t, first.LearnedCACert, "BEGIN CERTIFICATE")

	pinned := f.server(ctx, f.clusterUID).KubernetesData().LearnedCACert
	require.NotEmpty(t, pinned, "the learned bundle must be persisted")

	// A second check verifies against the pin rather than learning again, and
	// a real session still runs through it.
	second := checker.Check(ctx, f.server(ctx, f.clusterUID))
	require.True(t, second.OK, "the pinned connect failed: %s", second.Message)
	assert.False(t, second.CAPinned, "only the first connect pins")
	assert.Equal(t, pinned, f.server(ctx, f.clusterUID).KubernetesData().LearnedCACert)

	conn, err := f.connect(ctx)
	require.NoError(t, err, "a session must run on the learned pin")

	defer func() { _ = conn.Close(context.Background()) }()

	var got int
	require.NoError(t, conn.QueryRow(ctx, "SELECT 1").Scan(&got))
	assert.Equal(t, 1, got)
}
