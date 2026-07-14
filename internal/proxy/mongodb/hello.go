package mongodb

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Pinned wire-version window (contract §4). maxWireVersion 21 == MongoDB 7.0:
// modern enough for current drivers (Go driver v2 requires >= 7), low enough
// not to invite features we don't proxy.
const (
	minWireVersion int32 = 0
	maxWireVersion int32 = 21
)

// Static hello limits (contract §3).
const (
	maxBSONObjectSize            int32 = 16 * 1024 * 1024 // 16 MB
	maxMessageSizeBytes          int32 = 48000000         // 48 MB
	maxWriteBatchSize            int32 = 100000
	logicalSessionTimeoutMinutes int32 = 30
)

// helloDoc synthesizes the client-facing hello/isMaster reply (contract §3).
// It is generated from static values — never forwarded/rewritten from the
// upstream — because hello arrives before auth, i.e. before dbbat knows which
// target database the session is for. Presenting as a standalone (no hosts /
// setName / primary / me / topologyVersion) stops the driver dialing the real
// host directly (hard problem 2).
//
// name is the command the client sent ("hello" vs "isMaster"/"ismaster");
// request is the client's command document, inspected for helloOk and
// saslSupportedMechs echoes.
func (s *Session) helloDoc(name string, request bson.Raw) bson.D {
	doc := bson.D{}

	if name == "hello" {
		doc = append(doc, bson.E{Key: "isWritablePrimary", Value: true})
	} else {
		doc = append(doc, bson.E{Key: "ismaster", Value: true})
	}

	// Echo helloOk only when the client advertised it (handshake spec).
	if lookupBool(request, "helloOk") {
		doc = append(doc, bson.E{Key: "helloOk", Value: true})
	}

	doc = append(doc,
		bson.E{Key: "maxBsonObjectSize", Value: maxBSONObjectSize},
		bson.E{Key: "maxMessageSizeBytes", Value: maxMessageSizeBytes},
		bson.E{Key: "maxWriteBatchSize", Value: maxWriteBatchSize},
		bson.E{Key: "localTime", Value: bson.NewDateTimeFromTime(time.Now())},
		bson.E{Key: "logicalSessionTimeoutMinutes", Value: logicalSessionTimeoutMinutes},
		bson.E{Key: "connectionId", Value: s.connID},
		bson.E{Key: "minWireVersion", Value: minWireVersion},
		bson.E{Key: "maxWireVersion", Value: maxWireVersion},
		bson.E{Key: "readOnly", Value: false},
	)

	// If the client asked which SASL mechanisms fit its user, advertise the
	// mechanisms it can use (contract §3): SCRAM-SHA-256 when the named user has
	// a stored verifier, otherwise PLAIN. PLAIN is always offered as a fallback.
	if mechs := lookupString(request, "saslSupportedMechs"); mechs != "" {
		doc = append(doc, bson.E{Key: "saslSupportedMechs", Value: s.supportedMechsFor(mechs)})
	}

	// Wire compression (item 4): if the client offered a compressor we support
	// (zlib), echo it so the client may compress subsequent messages. dbbat then
	// mirrors — it compresses replies only after the client sends a compressed
	// frame (see Session.compressReplies).
	if clientOffersZlib(request) {
		doc = append(doc, bson.E{Key: "compression", Value: bson.A{"zlib"}})
	}

	// loadBalanced=true (MongoDB 5.0+): the client asks the server to identify
	// itself with a serviceId so it can pin cursors/transactions to this
	// connection. This is the clean topology story for an L4 proxy — a driver in
	// loadBalanced mode never tries to discover or dial the real host directly.
	if lookupBool(request, "loadBalanced") {
		doc = append(doc, bson.E{Key: "serviceId", Value: s.server.serviceID})
	}

	doc = append(doc, bson.E{Key: "ok", Value: 1.0})

	return doc
}

// supportedMechsFor answers a hello's saslSupportedMechs probe. The probe value
// is "<authSource>.<username>". SCRAM-SHA-256 is advertised first (drivers
// prefer it) when that user has a stored MongoDB SCRAM verifier; PLAIN is
// always offered as a fallback.
func (s *Session) supportedMechsFor(probe string) bson.A {
	username := ""
	if idx := indexByte([]byte(probe), '.'); idx >= 0 {
		username = probe[idx+1:]
	}

	if username != "" && s.userHasMongoVerifier(username) {
		return bson.A{"SCRAM-SHA-256", "PLAIN"}
	}

	return bson.A{"PLAIN"}
}

// userHasMongoVerifier reports whether the named dbbat user has a stored
// MongoDB SCRAM-SHA-256 verifier (populated on a password change after the
// feature shipped) — driving whether hello advertises SCRAM-SHA-256.
func (s *Session) userHasMongoVerifier(username string) bool {
	user, err := s.server.store.GetUserByUsername(s.ctx, username)
	if err != nil {
		return false
	}

	return user.MongoSCRAMCredentials() != nil
}

// clientOffersZlib reports whether a hello's compression array offers zlib, the
// one compressor dbbat supports.
func clientOffersZlib(request bson.Raw) bool {
	if request == nil {
		return false
	}

	arr, ok := request.Lookup("compression").ArrayOK()
	if !ok {
		return false
	}

	values, err := arr.Values()
	if err != nil {
		return false
	}

	for _, v := range values {
		if name, ok := v.StringValueOK(); ok {
			if _, supported := compressorIDForName(name); supported {
				return true
			}
		}
	}

	return false
}

// okDoc is the minimal success reply {ok: 1.0}.
func okDoc() bson.D {
	return bson.D{{Key: "ok", Value: 1.0}}
}

// buildInfoDoc is a minimal synthesized buildInfo reply, enough for a driver's
// pre-auth handshake to proceed.
func buildInfoDoc() bson.D {
	return bson.D{
		{Key: "version", Value: "7.0.0"},
		{Key: "versionArray", Value: bson.A{7, 0, 0, 0}},
		{Key: "maxWireVersion", Value: maxWireVersion},
		{Key: "ok", Value: 1.0},
	}
}
