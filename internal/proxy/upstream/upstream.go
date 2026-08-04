// Package upstream owns the single path dbbat takes from "a server row" to "an
// authenticated connection to that server". Both entry points use it: the
// proxies, which then MITM the connection they get back, and the connectivity
// check, which closes it immediately.
//
// Why a package of its own rather than internal/proxy/shared: shared is the
// grab-bag every protocol package *and* conncheck already import, and it stays
// deliberately dependency-light (net, ssh, store). The connectors here pull in
// pgproto3, go-mysql and go-ora, and nothing that only wants a byte counter or
// a SQL validator should inherit those. A dedicated package also names the
// invariant — one implementation of the ssl_mode policy and one implementation
// of each protocol login — which is the whole point of the split.
//
// Import direction: upstream imports shared (for the transport) and store; the
// protocol packages and conncheck import upstream. Nothing here imports a
// protocol package, so no cycle is possible.
//
// Two things this package does NOT deliver, both worth knowing before trusting
// the headline claim ("a green connectivity check proves the proxy can get in"):
//
//   - MongoDB's upstream login is implemented in internal/proxy/mongodb, not
//     here, because it is written on top of that package's OP_MSG codec, which
//     the whole MongoDB proxy is built from and which would have to be hoisted
//     wholesale to move the connector. It uses the same Plan from this package,
//     so the ssl_mode policy is still defined exactly once and the probe and the
//     proxy still run the same code; only the code's postal address differs.
//     conncheck calls it directly.
//
//   - Oracle is the one protocol where probe and proxy are genuinely NOT the
//     same code, and where they do not even agree on encryption. The proxy
//     relays the client's own TNS Connect descriptor byte for byte, so it has no
//     standalone login to share and no TLS at all: an Oracle session's upstream
//     leg is plaintext whatever the row's ssl_mode says (recorded honestly as
//     upstream_tls=false on the connection row). ConnectOracle, which only the
//     probe runs, gates TLS on Plan.RequiresTLS, so it encrypts under require
//     and verify-* and stays plaintext under the opportunistic modes. A green
//     check on an Oracle row at ssl_mode=require therefore proves more than the
//     proxy will actually do — the one real exception to the claim above.
//     Giving the Oracle proxy upstream TLS is out of scope here and would be a
//     change to its TNS relay, not to this package.
package upstream

import (
	"context"
	"net"
)

// DialFunc opens the raw transport to the target — directly, or through the SSH
// bastion chain. Connectors never dial themselves: the caller injects this so
// the proxy and the probe provably traverse the same tunnel.
type DialFunc func(ctx context.Context) (net.Conn, error)
