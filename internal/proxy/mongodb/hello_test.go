package mongodb

import (
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/store"
)

// buildOpMsgBody wraps a command document in an OP_MSG body (empty flags,
// one kind-0 section) — the shape dispatchPreAuthOpMsg's parseOpMsg expects
// as message.body, without the outer wire.go message header this test does
// not need.
func buildOpMsgBody(t *testing.T, doc bson.Raw) []byte {
	t.Helper()

	body := make([]byte, 0, 4+1+len(doc))
	body = append(body, 0, 0, 0, 0) // flagBits
	body = append(body, 0)          // kind 0 (body section)
	body = append(body, doc...)

	return body
}

func mongoRaw(t *testing.T, d bson.D) bson.Raw {
	t.Helper()

	raw, err := bson.Marshal(d)
	require.NoError(t, err)

	return raw
}

// TestHelloLoadBalanced verifies a loadBalanced=true hello gets the server's
// serviceId, while a plain hello does not (item 3).
func TestHelloLoadBalanced(t *testing.T) {
	t.Parallel()

	srv := &Server{serviceID: bson.NewObjectID()}
	s := &Session{server: srv, connID: 1}

	lb := s.helloDoc("hello", mongoRaw(t, bson.D{{Key: "hello", Value: 1}, {Key: "loadBalanced", Value: true}}))
	sid, ok := lookupServiceID(lb)
	require.True(t, ok, "loadBalanced hello must carry serviceId")
	assert.Equal(t, srv.serviceID, sid)

	plain := s.helloDoc("hello", mongoRaw(t, bson.D{{Key: "hello", Value: 1}}))
	_, ok = lookupServiceID(plain)
	assert.False(t, ok, "plain hello must not carry serviceId")
}

// TestHelloCompressionNegotiation verifies zlib is echoed only when offered, and
// unsupported compressors are declined (item 4).
func TestHelloCompressionNegotiation(t *testing.T) {
	t.Parallel()

	s := &Session{server: &Server{}, connID: 1}

	withZlib := s.helloDoc("hello", mongoRaw(t, bson.D{{Key: "hello", Value: 1}, {Key: "compression", Value: bson.A{"snappy", "zlib"}}}))
	assert.Equal(t, []string{"zlib"}, compressionList(withZlib))

	unsupported := s.helloDoc("hello", mongoRaw(t, bson.D{{Key: "hello", Value: 1}, {Key: "compression", Value: bson.A{"snappy", "zstd"}}}))
	assert.Nil(t, compressionList(unsupported), "unsupported compressors are not echoed")

	none := s.helloDoc("hello", mongoRaw(t, bson.D{{Key: "hello", Value: 1}}))
	assert.Nil(t, compressionList(none))
}

// TestClientAppNameFromHello covers extraction of client.application.name —
// the value a driver or shell (mongosh, a connection string's appName)
// declares in its hello, which dbbat now forwards to the upstream instead of
// the "" it used to send.
func TestClientAppNameFromHello(t *testing.T) {
	t.Parallel()

	withName := mongoRaw(t, bson.D{
		{Key: "hello", Value: 1},
		{Key: "client", Value: bson.D{
			{Key: "application", Value: bson.D{{Key: "name", Value: "mongosh"}}},
			{Key: "driver", Value: bson.D{{Key: "name", Value: "nodejs"}}},
		}},
	})
	assert.Equal(t, "mongosh", clientAppNameFromHello(withName))

	noClient := mongoRaw(t, bson.D{{Key: "hello", Value: 1}})
	assert.Empty(t, clientAppNameFromHello(noClient))

	assert.Empty(t, clientAppNameFromHello(nil))
}

// TestDispatchPreAuthOpMsgCapturesClientApplicationName exercises the OP_MSG
// hello path through dispatchPreAuth end to end (client conn included, via
// net.Pipe), confirming s.clientApplicationName is populated from the
// client's own declared appName — the field connectUpstream now folds into
// the upstream-facing name instead of sending "". This is also the "a client
// declaring appName=mongosh is seen upstream as ... for mongosh" case.
func TestDispatchPreAuthOpMsgCapturesClientApplicationName(t *testing.T) {
	t.Parallel()

	clientSide, proxySide := net.Pipe()
	t.Cleanup(func() { _ = clientSide.Close(); _ = proxySide.Close() })

	go func() { _, _ = io.Copy(io.Discard, clientSide) }()

	s := &Session{server: &Server{}, connID: 1, clientConn: proxySide}

	body := mongoRaw(t, bson.D{
		{Key: "hello", Value: 1},
		{Key: "client", Value: bson.D{{Key: "application", Value: bson.D{{Key: "name", Value: "mongosh"}}}}},
	})

	_, err := s.dispatchPreAuthOpMsg(&message{opCode: opCodeMsg, body: buildOpMsgBody(t, body)})
	require.NoError(t, err)

	assert.Equal(t, "mongosh", s.clientApplicationName)
}

// TestMongoAuthSourceOrDefault covers the item 2 default/override helper.
func TestMongoAuthSourceOrDefault(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "admin", (&store.Server{}).MongoAuthSourceOrDefault())

	withCustom := &store.Server{ProtocolData: &store.ServerProtocolData{MongoDB: &store.MongoDatabaseData{AuthSource: "services"}}}
	assert.Equal(t, "services", withCustom.MongoAuthSourceOrDefault())

	withEmpty := &store.Server{ProtocolData: &store.ServerProtocolData{MongoDB: &store.MongoDatabaseData{AuthSource: ""}}}
	assert.Equal(t, "admin", withEmpty.MongoAuthSourceOrDefault())
}

func lookupServiceID(doc bson.D) (bson.ObjectID, bool) {
	for _, e := range doc {
		if e.Key == "serviceId" {
			if oid, ok := e.Value.(bson.ObjectID); ok {
				return oid, true
			}
		}
	}

	return bson.ObjectID{}, false
}

func compressionList(doc bson.D) []string {
	for _, e := range doc {
		if e.Key != "compression" {
			continue
		}

		arr, ok := e.Value.(bson.A)
		if !ok {
			return nil
		}

		out := make([]string, 0, len(arr))
		for _, v := range arr {
			if str, ok := v.(string); ok {
				out = append(out, str)
			}
		}

		return out
	}

	return nil
}
