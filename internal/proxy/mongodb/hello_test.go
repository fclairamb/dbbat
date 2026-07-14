package mongodb

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/store"
)

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

// TestMongoAuthSourceOrDefault covers the item 2 default/override helper.
func TestMongoAuthSourceOrDefault(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "admin", (&store.Database{}).MongoAuthSourceOrDefault())

	withCustom := &store.Database{ProtocolData: &store.DatabaseProtocolData{MongoDB: &store.MongoDatabaseData{AuthSource: "services"}}}
	assert.Equal(t, "services", withCustom.MongoAuthSourceOrDefault())

	withEmpty := &store.Database{ProtocolData: &store.DatabaseProtocolData{MongoDB: &store.MongoDatabaseData{AuthSource: ""}}}
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
