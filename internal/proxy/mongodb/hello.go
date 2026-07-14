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

	// If the client asked which SASL mechanisms fit its user, advertise PLAIN
	// only (contract §3) so it falls through to our PLAIN negotiation.
	if lookupString(request, "saslSupportedMechs") != "" {
		doc = append(doc, bson.E{Key: "saslSupportedMechs", Value: bson.A{"PLAIN"}})
	}

	doc = append(doc, bson.E{Key: "ok", Value: 1.0})

	return doc
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
