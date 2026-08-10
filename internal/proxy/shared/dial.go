package shared

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/fclairamb/dbbat/internal/proxy/upstream"
	"github.com/fclairamb/dbbat/internal/store"
)

// dialTimeout bounds both the plain TCP dial and the SSH handshake.
const dialTimeout = 30 * time.Second

// ErrSSHHostKeyMismatch is returned when a bastion presents a host key that
// differs from the TOFU-pinned one recorded on first connect.
var ErrSSHHostKeyMismatch = errors.New("ssh: host key mismatch with pinned known_host_key")

// ErrBastionNotSSH is returned when a via_uid resolves to a non-ssh row.
var ErrBastionNotSSH = errors.New("ssh: via_uid does not reference an ssh server")

// ErrViaNotTunnel is returned when a via_uid resolves to a row that is neither
// an SSH bastion nor a Kubernetes cluster — i.e. not a dial path at all.
var ErrViaNotTunnel = errors.New("via_uid does not reference a tunnel server (ssh or kubernetes)")

// ErrKubernetesViaUnsupported is returned when an SSH bastion is itself placed
// behind a Kubernetes tunnel. The reverse (a cluster reached through a bastion)
// is supported; this direction would need a relay and is deliberately deferred.
var ErrKubernetesViaUnsupported = errors.New("kubernetes: an ssh bastion cannot be reached through a kubernetes tunnel")

// ErrNoSSHAuthMethod is returned when a bastion row has neither a private key
// nor a password to authenticate with.
var ErrNoSSHAuthMethod = errors.New("ssh: bastion has no usable auth method (private key or password)")

// ServerResolver resolves server rows and persists TOFU host keys. Satisfied by
// *store.Store; an interface so the dialer can be unit-tested with a fake.
type ServerResolver interface {
	GetServerByUID(ctx context.Context, uid uuid.UUID) (*store.Server, error)
	SetKnownHostKey(ctx context.Context, uid uuid.UUID, hostKey string) error
}

// Dialer opens upstream connections, tunneling through a *via* row when a
// server row's ViaUID is set. Two kinds of via row exist and are pooled the
// same way, keyed by server UID: an SSH bastion yields a shared *ssh.Client
// carrying one channel per session, and a Kubernetes cluster yields a shared
// port-forward tunnel carrying one stream pair per session. A dead entry of
// either kind is transparently rebuilt.
type Dialer struct {
	mu      sync.Mutex
	clients map[uuid.UUID]*ssh.Client
	tunnels map[uuid.UUID]*upstream.KubernetesTunnel
}

// NewDialer builds an empty Dialer with its own via pools.
func NewDialer() *Dialer {
	return &Dialer{
		clients: make(map[uuid.UUID]*ssh.Client),
		tunnels: make(map[uuid.UUID]*upstream.KubernetesTunnel),
	}
}

// defaultDialer is the process-wide pool shared by all four proxies.
var defaultDialer = NewDialer()

// DialUpstream dials srv's host:port using the process-wide pooled dialer.
// resolver loads the via chain and persists TOFU host keys; encryptionKey
// decrypts bastion SSH secrets.
func DialUpstream(ctx context.Context, resolver ServerResolver, encryptionKey []byte, srv *store.Server) (net.Conn, error) {
	return defaultDialer.DialUpstream(ctx, resolver, encryptionKey, srv)
}

// DialUpstream dials srv's host:port directly, or through srv.ViaUID's via
// chain when set — an SSH bastion (recursing for multi-hop jump hosts) or a
// Kubernetes cluster.
func (d *Dialer) DialUpstream(ctx context.Context, resolver ServerResolver, encryptionKey []byte, srv *store.Server) (net.Conn, error) {
	if srv.ViaUID == nil {
		addr := net.JoinHostPort(srv.Host, strconv.Itoa(srv.Port))

		var nd net.Dialer
		nd.Timeout = dialTimeout
		conn, err := nd.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("failed to dial upstream %s: %w", addr, err)
		}
		return conn, nil
	}

	via, err := resolver.GetServerByUID(ctx, *srv.ViaUID)
	if err != nil {
		return nil, fmt.Errorf("failed to load via server: %w", err)
	}

	if via.Protocol == store.ProtocolKubernetes {
		return d.dialViaKubernetes(ctx, resolver, encryptionKey, srv, *srv.ViaUID)
	}

	return d.dialViaSSH(ctx, resolver, encryptionKey, srv, *srv.ViaUID)
}

// dialViaSSH opens a direct-tcpip channel to the target through a pooled
// bastion client, dropping and rebuilding the client once if it turns out to be
// stale (the bastion silently dropped the pooled connection).
func (d *Dialer) dialViaSSH(
	ctx context.Context,
	resolver ServerResolver,
	encryptionKey []byte,
	srv *store.Server,
	viaUID uuid.UUID,
) (net.Conn, error) {
	addr := net.JoinHostPort(srv.Host, strconv.Itoa(srv.Port))

	client, err := d.sshClientFor(ctx, resolver, encryptionKey, viaUID, nil)
	if err != nil {
		return nil, err
	}

	conn, err := client.DialContext(ctx, "tcp", addr)
	if err != nil {
		// The pooled client may be stale (bastion dropped the connection).
		// Drop it and retry once with a fresh client.
		d.drop(viaUID)
		client, err = d.sshClientFor(ctx, resolver, encryptionKey, viaUID, nil)
		if err != nil {
			return nil, err
		}
		conn, err = client.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("failed to dial %s through ssh bastion: %w", addr, err)
		}
	}
	return conn, nil
}

// dialViaKubernetes opens a port-forward stream to the target through a pooled
// cluster tunnel.
//
// The target's Host is a pod name or `svc/<name>` in the tunnel's namespace,
// and its Port is the *container* port. Note what this does not do: unlike an
// SSH direct-tcpip channel, a port-forward reaches a pod's own port and nothing
// else, so a database merely routable from the cluster network is out of reach.
//
// The retry mirrors the SSH path but earns its keep differently: there is no
// shared stream connection to go stale here (each session upgrades its own),
// so what the retry buys is a **re-resolution** of svc→pod against a freshly
// built client. Pods move; a name that resolved a minute ago may name a pod
// that has since been rescheduled.
func (d *Dialer) dialViaKubernetes(
	ctx context.Context,
	resolver ServerResolver,
	encryptionKey []byte,
	srv *store.Server,
	viaUID uuid.UUID,
) (net.Conn, error) {
	tunnel, err := d.kubernetesTunnelFor(ctx, resolver, encryptionKey, viaUID)
	if err != nil {
		return nil, err
	}

	conn, err := tunnel.Dial(ctx, srv.Host, srv.Port)
	if err == nil {
		return conn, nil
	}

	d.drop(viaUID)

	tunnel, retryErr := d.kubernetesTunnelFor(ctx, resolver, encryptionKey, viaUID)
	if retryErr != nil {
		return nil, retryErr
	}

	conn, retryErr = tunnel.Dial(ctx, srv.Host, srv.Port)
	if retryErr != nil {
		return nil, fmt.Errorf("failed to reach %s:%d through the kubernetes tunnel: %w",
			srv.Host, srv.Port, retryErr)
	}

	return conn, nil
}

// kubernetesTunnelFor returns a pooled (or freshly built) tunnel for the
// cluster row identified by uid.
//
// The cluster row may itself carry a via_uid pointing at an SSH bastion, in
// which case every TCP connection to the API server is dialed through it. The
// reverse nesting is refused rather than silently mis-dialed.
func (d *Dialer) kubernetesTunnelFor(
	ctx context.Context,
	resolver ServerResolver,
	encryptionKey []byte,
	uid uuid.UUID,
) (*upstream.KubernetesTunnel, error) {
	d.mu.Lock()
	if t, ok := d.tunnels[uid]; ok {
		d.mu.Unlock()
		return t, nil
	}
	d.mu.Unlock()

	cluster, err := resolver.GetServerByUID(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("failed to load kubernetes cluster: %w", err)
	}
	if cluster.Protocol != store.ProtocolKubernetes {
		return nil, fmt.Errorf("%w: %s", ErrViaNotTunnel, uid)
	}

	// The ServiceAccount token rides in the row's encrypted password column, so
	// it travels the same AAD-bound path as a database password.
	if len(cluster.PasswordEncrypted) > 0 {
		if err := cluster.DecryptPassword(encryptionKey); err != nil {
			return nil, err
		}
	}

	cfg := upstream.KubernetesConfig{
		Host:      cluster.Host,
		Port:      cluster.Port,
		Token:     cluster.Password,
		Namespace: cluster.KubernetesNamespaceOrDefault(),
		UserAgent: kubernetesUserAgent,
	}

	if kd := cluster.KubernetesData(); kd != nil {
		cfg.CACert = kd.CACert
		cfg.InsecureSkipTLSVerify = kd.InsecureSkipTLSVerify
	}

	if cluster.ViaUID != nil {
		bastion, err := d.sshClientFor(ctx, resolver, encryptionKey, *cluster.ViaUID, nil)
		if err != nil {
			return nil, err
		}
		cfg.DialContext = func(dialCtx context.Context, network, addr string) (net.Conn, error) {
			return bastion.DialContext(dialCtx, network, addr)
		}
	}

	tunnel, err := upstream.NewKubernetesTunnel(cfg)
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	// Another goroutine may have won the race; keep whichever is already pooled
	// so concurrent sessions share one SPDY connection.
	if existing, ok := d.tunnels[uid]; ok {
		d.mu.Unlock()
		return existing, nil
	}
	d.tunnels[uid] = tunnel
	d.mu.Unlock()

	return tunnel, nil
}

// kubernetesUserAgent identifies dbbat in the cluster audit log, which is a
// large part of why riding the API server is worth it in the first place.
const kubernetesUserAgent = "dbbat/port-forward"

// ConnectBastion dials (or reuses a pooled connection to) the SSH bastion row
// identified by uid, completing the handshake and — on first connect — pinning
// the presented host key via resolver.SetKnownHostKey.
//
// It exists so a connectivity check can validate a `protocol: ssh` row on its
// own, with no database target behind it. Callers that want to force a real
// dial (rather than reuse a pooled client) must use a fresh Dialer.
func (d *Dialer) ConnectBastion(
	ctx context.Context,
	resolver ServerResolver,
	encryptionKey []byte,
	uid uuid.UUID,
) (*ssh.Client, error) {
	return d.sshClientFor(ctx, resolver, encryptionKey, uid, nil)
}

// ConnectKubernetes returns a pooled (or freshly built) tunnel for the
// Kubernetes cluster row identified by uid.
//
// It exists so a connectivity check can validate a `protocol: kubernetes` row
// on its own, with no database target behind it — the counterpart of
// ConnectBastion. Building the tunnel does no I/O; the caller is what probes.
func (d *Dialer) ConnectKubernetes(
	ctx context.Context,
	resolver ServerResolver,
	encryptionKey []byte,
	uid uuid.UUID,
) (*upstream.KubernetesTunnel, error) {
	return d.kubernetesTunnelFor(ctx, resolver, encryptionKey, uid)
}

// Close tears down every pooled via entry. Used by short-lived dialers
// (connectivity checks) so a probe does not leak an SSH connection.
func (d *Dialer) Close() {
	d.mu.Lock()
	for uid, c := range d.clients {
		_ = c.Close()
		delete(d.clients, uid)
	}
	// Kubernetes tunnels hold no socket of their own: the upgraded connection
	// is created per DialPod and owned by the returned conn, so forgetting the
	// tunnel is all the teardown there is.
	for uid := range d.tunnels {
		delete(d.tunnels, uid)
	}
	d.mu.Unlock()
}

// drop removes a (presumed dead) via entry from the pool, closing it when it
// owns a connection.
func (d *Dialer) drop(uid uuid.UUID) {
	d.mu.Lock()
	if c, ok := d.clients[uid]; ok {
		_ = c.Close()
		delete(d.clients, uid)
	}
	delete(d.tunnels, uid)
	d.mu.Unlock()
}

// sshClientFor returns a pooled (or freshly dialed) SSH client for the bastion
// identified by uid. seen guards against via_uid cycles during multi-hop
// resolution (the store validates this too, but the dialer must not loop).
func (d *Dialer) sshClientFor(
	ctx context.Context,
	resolver ServerResolver,
	encryptionKey []byte,
	uid uuid.UUID,
	seen map[uuid.UUID]bool,
) (*ssh.Client, error) {
	d.mu.Lock()
	if c, ok := d.clients[uid]; ok {
		d.mu.Unlock()
		return c, nil
	}
	d.mu.Unlock()

	if seen == nil {
		seen = make(map[uuid.UUID]bool)
	}
	if seen[uid] {
		return nil, ErrServerViaCycleDial
	}
	seen[uid] = true

	bastion, err := resolver.GetServerByUID(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("failed to load ssh bastion: %w", err)
	}
	if bastion.Protocol == store.ProtocolKubernetes {
		// The store lets this chain validate; the dialer is what has not grown
		// support for it. Say so precisely rather than "not an ssh server".
		return nil, fmt.Errorf("%w: %s", ErrKubernetesViaUnsupported, uid)
	}
	if bastion.Protocol != store.ProtocolSSH {
		return nil, fmt.Errorf("%w: %s", ErrBastionNotSSH, uid)
	}
	if err := bastion.DecryptSSHSecrets(encryptionKey); err != nil {
		return nil, err
	}
	// A bastion may authenticate by key only; decrypt the password just for the
	// password-auth path (an empty/absent ciphertext means no password auth).
	if len(bastion.PasswordEncrypted) > 0 {
		if err := bastion.DecryptPassword(encryptionKey); err != nil {
			return nil, err
		}
	}

	auths, err := sshAuthMethods(bastion)
	if err != nil {
		return nil, err
	}

	var recordedKey string
	cfg := &ssh.ClientConfig{
		User:            bastion.Username,
		Auth:            auths,
		HostKeyCallback: hostKeyCallback(bastion, &recordedKey),
		Timeout:         dialTimeout,
	}

	bastionAddr := net.JoinHostPort(bastion.Host, strconv.Itoa(bastion.Port))

	// Obtain the raw connection to the bastion: direct, or through its own
	// upstream bastion (multi-hop jump chain).
	var rawConn net.Conn
	if bastion.ViaUID == nil {
		var nd net.Dialer
		nd.Timeout = dialTimeout
		rawConn, err = nd.DialContext(ctx, "tcp", bastionAddr)
	} else {
		var parent *ssh.Client
		parent, err = d.sshClientFor(ctx, resolver, encryptionKey, *bastion.ViaUID, seen)
		if err != nil {
			return nil, err
		}
		rawConn, err = parent.DialContext(ctx, "tcp", bastionAddr)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to reach ssh bastion %s: %w", bastionAddr, err)
	}

	// ssh.NewClientConn has no context parameter, so a bastion that accepts the
	// TCP connection and then goes silent would block for cfg.Timeout regardless
	// of the caller's deadline. Project the context deadline onto the raw
	// connection for the duration of the handshake, then clear it — the pooled
	// client must not inherit a deadline.
	if deadline, ok := ctx.Deadline(); ok {
		_ = rawConn.SetDeadline(deadline)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(rawConn, bastionAddr, cfg)
	if err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("ssh handshake with %s failed: %w", bastionAddr, err)
	}

	_ = rawConn.SetDeadline(time.Time{})

	client := ssh.NewClient(sshConn, chans, reqs)

	// TOFU: persist the host key learned on first connect.
	if recordedKey != "" && (bastion.SSHData() == nil || bastion.SSHData().KnownHostKey == "") {
		if err := resolver.SetKnownHostKey(ctx, uid, recordedKey); err != nil {
			// Non-fatal: the connection works, we just failed to pin. Surface
			// via error so the caller can log, but keep the client usable by
			// pooling it first.
			d.pool(uid, client)
			return client, fmt.Errorf("connected but failed to pin ssh host key: %w", err)
		}
	}

	d.pool(uid, client)
	return client, nil
}

// pool stores client under uid, closing any client it displaces.
func (d *Dialer) pool(uid uuid.UUID, client *ssh.Client) {
	d.mu.Lock()
	if existing, ok := d.clients[uid]; ok && existing != client {
		_ = existing.Close()
	}
	d.clients[uid] = client
	d.mu.Unlock()
}

// ErrServerViaCycleDial mirrors store.ErrServerViaCycle for the dial path.
var ErrServerViaCycleDial = errors.New("ssh: via_uid chain forms a cycle")

// sshAuthMethods builds the SSH auth methods from the bastion's decrypted
// secrets: public-key (with optional passphrase) and/or password.
func sshAuthMethods(bastion *store.Server) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if sd := bastion.SSHData(); sd != nil && sd.PrivateKey != "" {
		var signer ssh.Signer
		var err error
		if sd.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(sd.PrivateKey), []byte(sd.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(sd.PrivateKey))
		}
		if err != nil {
			return nil, fmt.Errorf("failed to parse ssh private key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if bastion.Password != "" {
		methods = append(methods, ssh.Password(bastion.Password))
	}

	if len(methods) == 0 {
		return nil, ErrNoSSHAuthMethod
	}
	return methods, nil
}

// hostKeyCallback implements TOFU host-key verification. On first connect
// (no pinned key) it records the presented key into *recorded (persisted by the
// caller) and accepts. On subsequent connects it rejects any key that does not
// match the pinned known_host_key.
func hostKeyCallback(bastion *store.Server, recorded *string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		presented := string(ssh.MarshalAuthorizedKey(key))
		presented = trimTrailingNewline(presented)

		var pinned string
		if sd := bastion.SSHData(); sd != nil {
			pinned = sd.KnownHostKey
		}

		if pinned == "" {
			*recorded = presented
			return nil
		}
		if pinned != presented {
			return ErrSSHHostKeyMismatch
		}
		return nil
	}
}

// trimTrailingNewline removes a single trailing '\n' that
// ssh.MarshalAuthorizedKey appends, so pinned/presented comparison is stable.
func trimTrailingNewline(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		return s[:len(s)-1]
	}
	return s
}
