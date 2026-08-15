// Package conncheck validates that a configured server row is actually
// reachable and usable: that an SSH bastion accepts the stored key, and that a
// database target can be dialed (and authenticated against) — optionally
// through that bastion.
//
// It exists so provisioning is not write-and-hope: creating a bastion with a
// typo'd host, a wrong username or a key the bastion does not accept used to
// look exactly like success until some user's first real query failed.
//
// The result is deliberately staged: the stage is what tells an admin which
// field they got wrong. Secrets (private key, passphrase, password) are never
// echoed into the result or the logs.
package conncheck

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// DefaultTimeout bounds a whole check (bastion handshake + target dial + target
// auth). Kept below the shared dialer's own 30s per-dial timeout so a wedged
// host fails the HTTP request in bounded time rather than hanging on it.
const DefaultTimeout = 15 * time.Second

// Stage identifies how far the check got before it stopped. On success the
// stage is the last stage actually reached, so the caller can tell "tunnel and
// database both verified" from "tunnel verified, database auth not verified".
type Stage string

const (
	// StageConfig covers everything that fails before any packet is sent:
	// missing credentials, an unusable private key, a via_uid cycle.
	StageConfig Stage = "config"
	// StageBastionDial is the TCP dial of the SSH bastion (DNS, routing, firewall).
	StageBastionDial Stage = "bastion_dial"
	// StageBastionAuth is the SSH handshake: host-key verification and auth.
	StageBastionAuth Stage = "bastion_auth"
	// StageTargetDial is the TCP dial of the database target — through the
	// bastion when via_uid is set, direct otherwise.
	StageTargetDial Stage = "target_dial"
	// StageTargetAuth is the protocol-level handshake and login against the
	// database target.
	StageTargetAuth Stage = "target_auth"
	// StageClusterAPI is reaching the Kubernetes API server (DNS, routing,
	// firewall, and the TLS trust established by the pasted CA bundle).
	StageClusterAPI Stage = "cluster_api"
	// StageClusterAuth is the API server accepting the ServiceAccount token.
	StageClusterAuth Stage = "cluster_auth"
	// StageClusterRBAC is the SelfSubjectAccessReview on `pods/portforward`:
	// authenticated, but may we actually open a tunnel in this namespace.
	StageClusterRBAC Stage = "cluster_rbac"
	// StageClusterTarget is resolving the addressed pod (or the ready pod
	// behind `svc/<name>`) and confirming it is Ready.
	StageClusterTarget Stage = "cluster_target"
)

// Code is a stable, machine-readable classification of the failure within a
// stage. The UI keys its guidance off this, not off the message text.
type Code string

const (
	// CodeOK marks a successful check.
	CodeOK Code = "ok"
	// CodeDNSFailure means the hostname did not resolve.
	CodeDNSFailure Code = "dns_failure"
	// CodeTimeout means the dial or handshake exceeded the deadline.
	CodeTimeout Code = "timeout"
	// CodeUnreachable means the TCP connection was refused or the network is unreachable.
	CodeUnreachable Code = "unreachable"
	// CodeHostKeyMismatch means the bastion presented a host key differing from
	// the TOFU-pinned one.
	CodeHostKeyMismatch Code = "host_key_mismatch"
	// CodeAuthRejected means the bastion refused the offered credentials.
	CodeAuthRejected Code = "auth_rejected"
	// CodeBadPrivateKey means the stored private key could not be parsed (wrong
	// format, or a wrong/missing passphrase).
	CodeBadPrivateKey Code = "bad_private_key"
	// CodeNoAuthMethod means the bastion row carries neither key nor password.
	CodeNoAuthMethod Code = "no_auth_method"
	// CodeViaCycle means the via_uid chain loops.
	CodeViaCycle Code = "via_cycle"
	// CodeViaNotSSH means via_uid points at a row that is not an SSH bastion.
	CodeViaNotSSH Code = "via_not_ssh"
	// CodeHandshakeFailed is an SSH handshake failure that is not an auth
	// rejection (protocol mismatch, no common algorithm, ...).
	CodeHandshakeFailed Code = "handshake_failed"
	// CodeDBAuthFailed means the target accepted the connection but refused the
	// stored database credentials (or the database name).
	CodeDBAuthFailed Code = "db_auth_failed"
	// CodeDBHandshakeFailed means the target was reachable but the protocol
	// handshake did not complete (wrong port, TLS mismatch, not a database).
	CodeDBHandshakeFailed Code = "db_handshake_failed"
	// CodeMissingConfig means the row is missing a field the protocol needs
	// before anything can be dialed (an Oracle row with no service name).
	CodeMissingConfig Code = "missing_config"
	// CodeUnsupported means no protocol-level probe exists for this protocol;
	// reachability was verified but credentials were not.
	CodeUnsupported Code = "auth_not_verified"
	// CodeK8sForbidden means the ServiceAccount authenticated but RBAC denied
	// it — practically always a missing `pods/portforward` verb.
	CodeK8sForbidden Code = "k8s_forbidden"
	// CodeK8sTargetNotFound means no pod or service by that name exists in the
	// cluster row's namespace.
	CodeK8sTargetNotFound Code = "k8s_target_not_found"
	// CodeK8sTargetNotReady means the target exists but no ready pod backs it.
	CodeK8sTargetNotReady Code = "k8s_target_not_ready"
	// CodeK8sCAPinMismatch means the API server's certificate is not vouched
	// for by the CA dbbat pinned on first connect. Distinct from a plain
	// handshake failure because there *was* a working connect: this is the
	// cluster CA rotating, or somebody in the middle, and only an operator can
	// tell which.
	CodeK8sCAPinMismatch Code = "k8s_ca_pin_mismatch"
	// CodeInternal is an unclassified failure.
	CodeInternal Code = "internal_error"
	// CodeOracleServiceNameConflict is a *warning* code, not a failure: this
	// Oracle row's upstream service name is also claimed by rows spelling their
	// host differently, so a client connecting with the shared service name is
	// refused ORA-12514 even though this row itself checks out.
	CodeOracleServiceNameConflict Code = "oracle_service_name_conflict"
)

// Warning is a non-fatal configuration problem noticed while checking a row.
// The check itself can pass — the row is reachable and its credentials work —
// while something about how it sits next to its neighbors will bite a client
// later. Structured like a failure (a stable code plus text) so the UI keys off
// the code rather than the wording.
type Warning struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

// Result is the structured outcome of a connectivity check. It carries no
// secret material: only the stage reached, a machine-readable code, a
// human-readable message and (for SSH rows) the public host key.
type Result struct {
	// OK reports whether the check succeeded end to end.
	OK bool `json:"ok"`
	// Stage is the last stage reached (on failure: the stage that failed).
	Stage Stage `json:"stage"`
	// Code classifies the outcome within the stage; CodeOK on success.
	Code Code `json:"code"`
	// Message is a human-readable explanation, safe to show to an admin.
	Message string `json:"message"`
	// HostKeyPinned is true when this check performed the TOFU pin (first
	// successful connect to a bastion that had no known_host_key yet).
	HostKeyPinned bool `json:"host_key_pinned,omitempty"`
	// KnownHostKey is the bastion's public host key after the check. Public
	// challenge material, safe to return.
	KnownHostKey string `json:"ssh_known_host_key,omitempty"`
	// CAPinned is the Kubernetes counterpart of HostKeyPinned: true when this
	// check performed the TOFU pin (first successful connect to a cluster row
	// that supplied no CA bundle and had learned none yet).
	CAPinned bool `json:"k8s_ca_pinned,omitempty"`
	// LearnedCACert is the cluster's TOFU-learned CA bundle after the check.
	// Public challenge material, like the host key; empty when the row supplied
	// its own bundle, which always wins.
	LearnedCACert string `json:"k8s_learned_ca_cert,omitempty"`
	// Warnings are problems that do not make the check fail but that an admin
	// needs to know about. They are reported whatever OK says: a row can be
	// perfectly reachable and still be unreachable *by service name*.
	Warnings []Warning `json:"warnings,omitempty"`
	// DurationMs is how long the whole check took.
	DurationMs int64 `json:"duration_ms"`
}

// Checker runs connectivity checks. It owns no long-lived state: every check
// builds a fresh SSH dialer so a pooled, already-open bastion client from the
// live proxy pool can never make a broken configuration look healthy.
type Checker struct {
	resolver      shared.ServerResolver
	encryptionKey []byte
	timeout       time.Duration
}

// New builds a Checker over the given resolver (normally *store.Store) and
// master encryption key.
func New(resolver shared.ServerResolver, encryptionKey []byte) *Checker {
	return &Checker{resolver: resolver, encryptionKey: encryptionKey, timeout: DefaultTimeout}
}

// WithTimeout returns a copy of the checker bounded by d. Callers serving an
// HTTP request use it to stay inside their own write timeout.
func (c *Checker) WithTimeout(d time.Duration) *Checker {
	cp := *c
	cp.timeout = d

	return &cp
}

// Check validates srv: an SSH bastion handshake for `protocol: ssh` rows, or a
// target dial (through the bastion chain when via_uid is set) plus a
// protocol-level login for database rows.
//
// It never returns an error: a failed check is a successful call with OK=false,
// because the staged failure is the answer the caller asked for.
func (c *Checker) Check(ctx context.Context, srv *store.Server) Result {
	start := time.Now()

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	dialer := shared.NewDialer()
	defer dialer.Close()

	res := c.check(ctx, dialer, srv)
	res.Warnings = append(res.Warnings, c.oracleServiceNameWarnings(ctx, srv)...)
	res.DurationMs = time.Since(start).Milliseconds()

	return res
}

// oracleServiceNameLister is the optional half of the resolver: a store can tell
// the checker which other rows claim this row's upstream Oracle service name.
//
// Optional, rather than a method on shared.ServerResolver, because every proxy's
// dial fake implements that interface and none of them needs a fleet query — the
// checker asks for the extra method and simply says nothing when the resolver
// cannot answer.
type oracleServiceNameLister interface {
	GetOracleServiceNameConflict(
		ctx context.Context,
		serviceName string,
	) (*store.OracleServiceNameConflict, bool, error)
}

// oracleServiceNameWarnings reports an Oracle row whose upstream service name is
// also claimed by rows pointing at a different `host:port`.
//
// This is the visible half of a dormant trap: the proxy compares candidate
// upstreams textually (internal/proxy/oracle/session.go, resolveDatabase), so
// two spellings of one machine — a CNAME here, the A-record there — make every
// connect that arrives with the shared service name fail ORA-12514, while each
// row on its own dials and logs in fine. Nothing about *this* row is broken,
// which is exactly why it needs saying out loud.
//
// Only the dedicated service-name column counts, matching what the proxy's
// candidate lookup keys off.
func (c *Checker) oracleServiceNameWarnings(ctx context.Context, srv *store.Server) []Warning {
	if srv == nil || srv.Protocol != store.ProtocolOracle {
		return nil
	}

	if srv.OracleServiceName == nil || *srv.OracleServiceName == "" {
		return nil
	}

	lister, ok := c.resolver.(oracleServiceNameLister)
	if !ok {
		return nil
	}

	conflict, found, err := lister.GetOracleServiceNameConflict(ctx, *srv.OracleServiceName)
	if err != nil || !found {
		return nil
	}

	return []Warning{{Code: CodeOracleServiceNameConflict, Message: conflict.Describe()}}
}

// check runs the protocol-appropriate check body against a caller-owned dialer.
func (c *Checker) check(ctx context.Context, dialer *shared.Dialer, srv *store.Server) Result {
	switch {
	case srv.IsSSH():
		return c.checkBastion(ctx, dialer, srv.UID)
	case srv.IsKubernetes():
		return c.checkCluster(ctx, dialer, srv)
	}

	// A target behind a cluster row is pre-flighted against the API server, so
	// the stage names the cluster problem instead of blaming the database.
	if srv.ViaUID != nil {
		if via, err := c.resolver.GetServerByUID(ctx, *srv.ViaUID); err == nil && via.IsKubernetes() {
			if res := c.checkKubernetesTarget(ctx, dialer, srv, *srv.ViaUID); res != nil {
				return *res
			}
		}
	}

	return c.checkTarget(ctx, dialer, srv)
}

// checkBastion dials and handshakes an SSH bastion row, pinning its host key on
// first success.
func (c *Checker) checkBastion(ctx context.Context, dialer *shared.Dialer, uid uuid.UUID) Result {
	// Snapshot the pinned key before connecting so we can report whether *this*
	// check performed the TOFU pin.
	var hadPin bool
	if before, err := c.resolver.GetServerByUID(ctx, uid); err == nil {
		if sd := before.SSHData(); sd != nil && sd.KnownHostKey != "" {
			hadPin = true
		}
	}

	client, err := dialer.ConnectBastion(ctx, c.resolver, c.encryptionKey, uid)
	if client != nil {
		defer func() { _ = client.Close() }()
	}

	if err != nil {
		return classifySSHError(err)
	}

	res := Result{
		OK:      true,
		Stage:   StageBastionAuth,
		Code:    CodeOK,
		Message: "SSH bastion reachable and the stored credentials were accepted",
	}

	// Re-read the row: sshClientFor persists the TOFU pin as part of a
	// successful first connect.
	if after, err := c.resolver.GetServerByUID(ctx, uid); err == nil {
		if sd := after.SSHData(); sd != nil {
			res.KnownHostKey = sd.KnownHostKey
			res.HostKeyPinned = !hadPin && sd.KnownHostKey != ""
		}
	}

	if res.HostKeyPinned {
		res.Message = "SSH bastion reachable, credentials accepted, host key pinned"
	}

	return res
}

// checkTarget dials a database target (through its bastion chain when via_uid
// is set) and, when a probe exists for the protocol, authenticates against it.
func (c *Checker) checkTarget(ctx context.Context, dialer *shared.Dialer, srv *store.Server) Result {
	probe := probeFor(srv.Protocol)

	if probe == nil {
		// No protocol-level probe: verify TCP reachability only, and say so.
		conn, err := dialer.DialUpstream(ctx, c.resolver, c.encryptionKey, srv)
		if err != nil {
			return classifyDialError(err, srv.ViaUID != nil)
		}
		_ = conn.Close()

		return Result{
			OK:      true,
			Stage:   StageTargetDial,
			Code:    CodeUnsupported,
			Message: "target reachable; dbbat has no login probe for protocol " + srv.Protocol + ", so credentials were not verified",
		}
	}

	// Decrypt the target's own password for the login probe. Bastion secrets are
	// decrypted inside the shared dialer.
	// An empty ciphertext means "no password stored" — a legitimate configuration
	// (trust/peer auth), not a decryption failure.
	if len(srv.PasswordEncrypted) > 0 {
		if err := srv.DecryptPassword(c.encryptionKey); err != nil {
			return Result{
				Stage:   StageConfig,
				Code:    CodeInternal,
				Message: "failed to decrypt the stored database password",
			}
		}
	}

	// Track the dial separately from the login so a refused TCP connection is
	// reported as target_dial, not as an auth failure. Conns are kept so the
	// timeout path can force them shut: a connection tunneled over SSH does not
	// support deadlines (golang.org/x/crypto/ssh channels reject SetDeadline),
	// so a database library blocked on a read cannot be unstuck by the context
	// alone — closing the transport underneath it is what unblocks it.
	var (
		mu      sync.Mutex
		dialErr error
		conns   []net.Conn
	)

	dial := func(dialCtx context.Context) (net.Conn, error) {
		conn, err := dialer.DialUpstream(dialCtx, c.resolver, c.encryptionKey, srv)

		mu.Lock()
		if err != nil {
			dialErr = err
		} else {
			conns = append(conns, conn)
		}
		mu.Unlock()

		return conn, err
	}

	done := make(chan error, 1)

	go func() { done <- runProbe(ctx, srv, dial, probe) }()

	var probeErr error

	select {
	case probeErr = <-done:
	case <-ctx.Done():
		mu.Lock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		mu.Unlock()

		// Give the probe a moment to unwind on the closed transport; either way
		// the answer is a timeout.
		select {
		case <-done:
		case <-time.After(time.Second):
		}

		return Result{
			Stage:   StageTargetAuth,
			Code:    CodeTimeout,
			Message: "the target accepted the connection but the database handshake did not complete in time",
		}
	}

	if probeErr != nil {
		mu.Lock()
		failedDial := dialErr
		mu.Unlock()

		if failedDial != nil {
			return classifyDialError(failedDial, srv.ViaUID != nil)
		}

		return classifyTargetError(probeErr)
	}

	return Result{
		OK:      true,
		Stage:   StageTargetAuth,
		Code:    CodeOK,
		Message: "target reachable and the stored credentials were accepted",
	}
}

// classifySSHError maps a bastion connect failure onto a stage + code. The
// distinction between "could not reach the host" and "the host refused the
// key" is the whole point of the check: they point at different config fields.
func classifySSHError(err error) Result {
	switch {
	case errors.Is(err, shared.ErrNoSSHAuthMethod):
		return Result{
			Stage:   StageConfig,
			Code:    CodeNoAuthMethod,
			Message: "the bastion has no usable credentials: set an SSH private key or a password",
		}
	case errors.Is(err, shared.ErrServerViaCycleDial):
		return Result{
			Stage:   StageConfig,
			Code:    CodeViaCycle,
			Message: "the via_uid chain forms a cycle",
		}
	case errors.Is(err, shared.ErrBastionNotSSH):
		return Result{
			Stage:   StageConfig,
			Code:    CodeViaNotSSH,
			Message: "via_uid does not reference an SSH server",
		}
	case errors.Is(err, shared.ErrSSHHostKeyMismatch):
		return Result{
			Stage: StageBastionAuth,
			Code:  CodeHostKeyMismatch,
			Message: "the bastion presented a host key that does not match the pinned known_host_key — " +
				"either the host changed, or the connection is being intercepted",
		}
	case isBadPrivateKey(err):
		return Result{
			Stage:   StageConfig,
			Code:    CodeBadPrivateKey,
			Message: "the stored SSH private key could not be parsed (wrong format, or a missing/incorrect passphrase)",
		}
	}

	// Handshake-stage failures carry the "ssh handshake" prefix from the dialer;
	// anything earlier is a dial-stage failure.
	if strings.Contains(err.Error(), "ssh handshake") {
		// A handshake that stalls (host accepts the TCP connection then goes
		// silent) is a timeout, not an auth problem — say so.
		if netRes := classifyNetworkError(err, StageBastionAuth); netRes.Code != CodeInternal {
			return netRes
		}

		if isAuthRejection(err) {
			return Result{
				Stage:   StageBastionAuth,
				Code:    CodeAuthRejected,
				Message: "the bastion refused the credentials: check the username and the SSH private key or password",
			}
		}

		return Result{
			Stage:   StageBastionAuth,
			Code:    CodeHandshakeFailed,
			Message: "the SSH handshake with the bastion failed: " + sanitize(err),
		}
	}

	res := classifyNetworkError(err, StageBastionDial)
	if res.Code == CodeInternal {
		res.Message = "could not reach the SSH bastion: " + sanitize(err)
	}

	return res
}

// classifyDialError maps a target dial failure, distinguishing a failure inside
// the tunnel from a direct dial failure.
func classifyDialError(err error, viaTunnel bool) Result {
	// A dial "through" a via row can fail because the via row itself is
	// unreachable or refuses us — report that as a via-stage failure so the
	// admin fixes the tunnel, not the target.
	if viaTunnel && isKubernetesStage(err) {
		return classifyKubernetesError(err, StageClusterAPI)
	}

	if viaTunnel && isBastionStage(err) {
		return classifySSHError(err)
	}

	stage := StageTargetDial

	res := classifyNetworkError(err, stage)
	if res.Code == CodeInternal {
		if viaTunnel {
			res.Message = "the SSH tunnel is up, but the target could not be reached through it: " + sanitize(err)
		} else {
			res.Message = "could not reach the target: " + sanitize(err)
		}
	}

	return res
}

// isBastionStage reports whether a dial error originates from establishing the
// bastion connection rather than from the target dial behind it.
func isBastionStage(err error) bool {
	if errors.Is(err, shared.ErrNoSSHAuthMethod) ||
		errors.Is(err, shared.ErrServerViaCycleDial) ||
		errors.Is(err, shared.ErrBastionNotSSH) ||
		errors.Is(err, shared.ErrSSHHostKeyMismatch) ||
		isBadPrivateKey(err) {
		return true
	}

	msg := err.Error()

	return strings.Contains(msg, "ssh handshake") ||
		strings.Contains(msg, "failed to reach ssh bastion") ||
		strings.Contains(msg, "failed to load ssh bastion")
}

// classifyNetworkError maps net-level failures (DNS, timeout, refusal) onto
// codes. Returns CodeInternal when it cannot tell, leaving the message to the
// caller.
func classifyNetworkError(err error, stage Stage) Result {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return Result{
			Stage:   stage,
			Code:    CodeDNSFailure,
			Message: "the hostname could not be resolved: " + dnsErr.Name,
		}
	}

	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return Result{
			Stage:   stage,
			Code:    CodeTimeout,
			Message: "the connection timed out: the host is unreachable or a firewall is dropping the packets",
		}
	}

	if isRefusedOrUnreachable(err) {
		return Result{
			Stage:   stage,
			Code:    CodeUnreachable,
			Message: "the connection was refused: check the host and port",
		}
	}

	return Result{Stage: stage, Code: CodeInternal}
}

// classifyTargetError maps a protocol-level login failure. Auth rejections are
// distinguished from handshake failures because they point at different fields.
func classifyTargetError(err error) Result {
	// A panic is unambiguous and must not be run past the network/auth
	// heuristics below, which would dress a dbbat bug up as a problem with the
	// admin's server row.
	if errors.Is(err, errProbePanic) {
		return Result{
			Stage: StageTargetAuth,
			Code:  CodeInternal,
			Message: "the connectivity check crashed while talking to this target — " +
				"this is a dbbat bug, not a problem with the server row; " +
				"the stack trace is in the server log",
		}
	}

	// An incomplete row is caught before any packet is sent, so it belongs to
	// the config stage — reporting it at target_auth would send the admin
	// looking at the network.
	if errors.Is(err, errMissingConfig) {
		return Result{
			Stage:   StageConfig,
			Code:    CodeMissingConfig,
			Message: sanitize(err),
		}
	}

	if res := classifyNetworkError(err, StageTargetAuth); res.Code != CodeInternal {
		// A timeout mid-handshake is still a handshake problem, but the network
		// classification is the more useful message.
		return res
	}

	if isDBAuthRejection(err) {
		return Result{
			Stage:   StageTargetAuth,
			Code:    CodeDBAuthFailed,
			Message: "the database refused the stored credentials: " + sanitize(err),
		}
	}

	return Result{
		Stage:   StageTargetAuth,
		Code:    CodeDBHandshakeFailed,
		Message: "the target was reachable but the database handshake failed: " + sanitize(err),
	}
}

// errProbePanic marks a probe that panicked instead of returning an error.
var errProbePanic = errors.New("conncheck: probe panicked")

// runProbe executes p and converts a panic into an error.
//
// The probes drive third-party client libraries — go-ora, go-mysql, the Mongo
// codec, pgproto3 — against whatever host an operator pointed a server row at,
// and those libraries are not hardened against a malformed handshake. go-ora's
// newAcceptPacketFromData indexes into a TNS Accept packet without a length
// check and panics outright on a short one; the scheduled Oracle integration
// run hit exactly that against an ordinary test container, so it does not take
// a hostile server to reach it.
//
// This runs on its own goroutine, with no caller to unwind into. Without the
// recover such a panic does not fail the check — it takes the whole proxy
// process down, dropping every live session on every protocol, because an
// admin pressed "test connection" (or passed test_connection: true) on a host
// having a bad moment. A connectivity check must never be able to do that, and
// a panic is simply a failed probe.
func runProbe(ctx context.Context, srv *store.Server, dial dialFunc, p probe) (err error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}

		// The stack is the only way to identify the offending library frame,
		// and it does not breach this package's no-secrets rule: Go renders
		// argument words, not string contents, so a stored password passed to
		// a client library never appears in the trace.
		slog.ErrorContext(ctx, "connectivity probe panicked",
			slog.String("server_uid", srv.UID.String()),
			slog.String("protocol", srv.Protocol),
			slog.Any("panic", r),
			slog.String("stack", string(debug.Stack())))

		err = fmt.Errorf("%w: %v", errProbePanic, r)
	}()

	return p(ctx, srv, dial)
}

// isTimeout reports whether err is a net.Error signaling a timeout.
func isTimeout(err error) bool {
	var netErr net.Error

	return errors.As(err, &netErr) && netErr.Timeout()
}

// isRefusedOrUnreachable reports whether err is a connection refusal or an
// unreachable network. Matched on text because the syscall errno constants
// differ per platform and are not worth a build-tagged file here.
func isRefusedOrUnreachable(err error) bool {
	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "no route to host") ||
		strings.Contains(msg, "connection reset by peer")
}

// isBadPrivateKey reports whether err came from parsing the stored private key.
func isBadPrivateKey(err error) bool {
	return strings.Contains(err.Error(), "failed to parse ssh private key")
}

// isAuthRejection reports whether an SSH handshake failure was the server
// refusing our credentials rather than a lower-level protocol problem.
func isAuthRejection(err error) bool {
	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "unable to authenticate") ||
		strings.Contains(msg, "no supported methods remain") ||
		strings.Contains(msg, "permission denied")
}

// oracleAuthErrorRe matches the Oracle error codes that mean "the credentials
// or the account are the problem", in both renderings go-ora produces: the
// server's own zero-padded text (`ORA-01017: invalid username/password`) and
// go-ora's internal table for listener refusals, which pads nothing
// (`ORA-1017`). Listener/service errors (12514, 12505, 12541, ...) are
// deliberately absent: those are handshake failures pointing at the host, port
// or service name, not at the credentials.
var oracleAuthErrorRe = regexp.MustCompile(`ora-0*(1017|1005|1004|1045|1031|9911|28000|28001|28009)\b`)

// isDBAuthRejection reports whether a database handshake failure was a
// credential/permission rejection. Covers the PostgreSQL SQLSTATE class 28
// wording, MySQL 1045/1044, the MongoDB SCRAM failure text, and the Oracle
// ORA- codes above.
func isDBAuthRejection(err error) bool {
	msg := strings.ToLower(err.Error())

	if oracleAuthErrorRe.MatchString(msg) {
		return true
	}

	for _, needle := range []string{
		"password authentication failed",
		"authentication failed",
		// SQL Server 18456 ("Login failed for user") and 4060 ("Cannot open
		// database requested by the login"), both of which mean the stored
		// credentials are the problem rather than the network.
		"login failed",
		"cannot open database",
		"auth failed",
		"access denied",
		"role \"", // 28000: role does not exist
		"does not exist",
		"permission denied",
		"not authorized",
		"1045",
		"1044",
		"28p01",
		"28000",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}

	return false
}

// sanitize renders an error for display. Errors on these paths come from the
// network stack and the protocol libraries and never embed dbbat's stored
// secrets, but the message is bounded so a hostile upstream cannot use the
// admin UI as an unbounded output channel.
func sanitize(err error) string {
	const maxLen = 300

	msg := strings.TrimSpace(err.Error())
	msg = strings.ReplaceAll(msg, "\n", " ")

	if len(msg) > maxLen {
		msg = msg[:maxLen] + "…"
	}

	return msg
}
