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

	"github.com/fclairamb/dbbat/internal/store"
)

// dialTimeout bounds both the plain TCP dial and the SSH handshake.
const dialTimeout = 30 * time.Second

// ErrSSHHostKeyMismatch is returned when a bastion presents a host key that
// differs from the TOFU-pinned one recorded on first connect.
var ErrSSHHostKeyMismatch = errors.New("ssh: host key mismatch with pinned known_host_key")

// ServerResolver resolves server rows and persists TOFU host keys. Satisfied by
// *store.Store; an interface so the dialer can be unit-tested with a fake.
type ServerResolver interface {
	GetServerByUID(ctx context.Context, uid uuid.UUID) (*store.Server, error)
	SetKnownHostKey(ctx context.Context, uid uuid.UUID, hostKey string) error
}

// Dialer opens upstream connections, tunneling through SSH bastions when a
// server row's ViaUID is set. It pools one *ssh.Client per bastion (keyed by
// server UID) so that N concurrent proxy sessions multiplex over a single SSH
// connection; a dead client is transparently reconnected.
type Dialer struct {
	mu      sync.Mutex
	clients map[uuid.UUID]*ssh.Client
}

// NewDialer builds an empty Dialer with its own bastion pool.
func NewDialer() *Dialer {
	return &Dialer{clients: make(map[uuid.UUID]*ssh.Client)}
}

// defaultDialer is the process-wide pool shared by all four proxies.
var defaultDialer = NewDialer()

// DialUpstream dials srv's host:port using the process-wide pooled dialer.
// resolver loads the via chain and persists TOFU host keys; encryptionKey
// decrypts bastion SSH secrets.
func DialUpstream(ctx context.Context, resolver ServerResolver, encryptionKey []byte, srv *store.Server) (net.Conn, error) {
	return defaultDialer.DialUpstream(ctx, resolver, encryptionKey, srv)
}

// DialUpstream dials srv's host:port directly, or through srv.ViaUID's SSH
// bastion chain when set (recursing for multi-hop jump hosts).
func (d *Dialer) DialUpstream(ctx context.Context, resolver ServerResolver, encryptionKey []byte, srv *store.Server) (net.Conn, error) {
	addr := net.JoinHostPort(srv.Host, strconv.Itoa(srv.Port))

	if srv.ViaUID == nil {
		var nd net.Dialer
		nd.Timeout = dialTimeout
		conn, err := nd.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("failed to dial upstream %s: %w", addr, err)
		}
		return conn, nil
	}

	client, err := d.sshClientFor(ctx, resolver, encryptionKey, *srv.ViaUID, nil)
	if err != nil {
		return nil, err
	}

	conn, err := client.DialContext(ctx, "tcp", addr)
	if err != nil {
		// The pooled client may be stale (bastion dropped the connection).
		// Drop it and retry once with a fresh client.
		d.drop(*srv.ViaUID)
		client, err = d.sshClientFor(ctx, resolver, encryptionKey, *srv.ViaUID, nil)
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

// drop removes a (presumed dead) client from the pool and closes it.
func (d *Dialer) drop(uid uuid.UUID) {
	d.mu.Lock()
	if c, ok := d.clients[uid]; ok {
		_ = c.Close()
		delete(d.clients, uid)
	}
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
	if bastion.Protocol != store.ProtocolSSH {
		return nil, fmt.Errorf("via_uid %s is not an ssh server", uid)
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

	sshConn, chans, reqs, err := ssh.NewClientConn(rawConn, bastionAddr, cfg)
	if err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("ssh handshake with %s failed: %w", bastionAddr, err)
	}
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
		return nil, errors.New("ssh bastion has no usable auth method (private key or password)")
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
