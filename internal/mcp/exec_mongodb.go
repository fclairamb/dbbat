package mcp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Errors an agent gets back for a MongoDB "statement" it wrote wrong. They are
// deliberately explicit: a model that guessed the shape has to be able to fix
// it from the message alone.
var (
	// ErrMongoCommandSyntax means the text is not `<command> <extended JSON>`.
	ErrMongoCommandSyntax = errors.New(
		`a MongoDB statement is "<command> <extended-JSON document>", e.g. find {"find":"users","limit":10}`)
	// ErrMongoCommandEmpty means the document carried no command.
	ErrMongoCommandEmpty = errors.New("the MongoDB command document is empty")
	// ErrMongoCommandMismatch means the leading word is not the document's
	// first key, which is what MongoDB uses to name the command.
	ErrMongoCommandMismatch = errors.New(
		"the command name must be the first key of the command document")
)

// mongoServerSelectionTimeout bounds the handshake with our own listener.
// Reaching loopback is either immediate or broken.
const mongoServerSelectionTimeout = 10 * time.Second

// executeMongoDB runs one MongoDB command against the MongoDB proxy listener.
//
// MongoDB has no SQL, so `sql` carries the command in exactly the form dbbat
// itself renders it into `/queries`: `<command> <extended JSON>`. That is the
// deliberate choice over a second `run_command` tool — it keeps the tool
// surface at four, and it keeps an operator's approval patterns meaning the
// same thing whether the command came from mongosh or from an agent.
//
// Unlike the other loopback clients this one is **not** plaintext. The MongoDB
// listener terminates TLS and authenticates with SASL PLAIN, which sends the
// `dbb_` key in the clear inside the tunnel, so the tunnel has to exist. The
// certificate is the proxy's own auto-generated self-signed one and the dial
// never leaves 127.0.0.1, so it is not verified — that exemption is scoped to
// this dial and exists nowhere else in the package.
func executeMongoDB(ctx context.Context, addr string, req ExecRequest) (*QueryResult, error) {
	command, doc, err := parseMongoStatement(req.SQL)
	if err != nil {
		return nil, err
	}

	opts := options.Client().
		SetHosts([]string{addr}).
		SetAuth(options.Credential{
			AuthMechanism: "PLAIN",
			// authSource carries the *dbbat entry's* name: it is how the proxy
			// resolves which database this session is for.
			AuthSource: req.Database,
			Username:   req.Username,
			Password:   req.APIKey,
		}).
		// InsecureSkipVerify is deliberate and scoped: see the doc comment.
		SetTLSConfig(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}).
		// dbbat presents a standalone server; without this the driver would run
		// topology discovery and dial whatever the upstream advertises.
		SetDirect(true).
		SetAppName(mcpApplicationName).
		SetServerSelectionTimeout(mongoServerSelectionTimeout)
		// Deliberately no SetTimeout: an approval hold parks the command on a
		// human for as long as it takes, and a driver-side operation clock would
		// cancel it. The execution context (ExecutionMaxLifetime) is the bound,
		// and canceling it closes the socket — the proxy's ordinary
		// client-disconnect path, which ends a hold as `abandoned`.

	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("connecting to the dbbat MongoDB proxy: %w", err)
	}

	defer func() { _ = client.Disconnect(context.WithoutCancel(ctx)) }()

	// The command's own `$db` is forwarded verbatim, so it has to name the
	// database as the *upstream* knows it, not as dbbat lists it.
	name := req.UpstreamDatabase
	if name == "" {
		name = req.Database
	}

	database := client.Database(name)

	if mongoCommandReturnsCursor(command) {
		return mongoRunCursor(ctx, database, doc, req.MaxRows)
	}

	return mongoRunCommand(ctx, database, doc)
}

// parseMongoStatement splits `<command> <extended JSON>` into the command name
// and the document to run.
//
// A bare document is accepted too — MongoDB names a command by its document's
// first key, so the leading word is a readability affordance, and when it is
// present it has to agree.
func parseMongoStatement(text string) (string, bson.D, error) {
	trimmed := strings.TrimSpace(text)

	start := strings.IndexByte(trimmed, '{')
	if start < 0 {
		return "", nil, ErrMongoCommandSyntax
	}

	name := strings.TrimSpace(trimmed[:start])

	var doc bson.D

	if err := bson.UnmarshalExtJSON([]byte(trimmed[start:]), false, &doc); err != nil {
		return "", nil, fmt.Errorf("%w: %w", ErrMongoCommandSyntax, err)
	}

	// The driver appends `$db` itself from the connection, and the proxy checks
	// it against the session's grant. A second one in the body would be a
	// duplicate field on the wire.
	doc = dropMongoKey(doc, "$db")

	if len(doc) == 0 {
		return "", nil, ErrMongoCommandEmpty
	}

	if name != "" && name != doc[0].Key {
		return "", nil, fmt.Errorf("%w: got %q, document starts with %q", ErrMongoCommandMismatch, name, doc[0].Key)
	}

	return doc[0].Key, doc, nil
}

// dropMongoKey removes a top-level key, preserving the order of the rest.
func dropMongoKey(doc bson.D, key string) bson.D {
	out := make(bson.D, 0, len(doc))

	for _, element := range doc {
		if element.Key == key {
			continue
		}

		out = append(out, element)
	}

	return out
}

// mongoCommandReturnsCursor reports whether a command answers with a cursor
// rather than a single reply document.
//
// The list is closed on purpose. Guessing wrong the other way — running an
// insert through RunCommandCursor — would execute the write and *then* fail to
// parse the reply, which is the one outcome an agent must never see.
func mongoCommandReturnsCursor(command string) bool {
	switch command {
	case "find", "aggregate", "listCollections", "listIndexes":
		return true
	default:
		return false
	}
}

// mongoRunCursor runs a cursor-returning command and reads at most maxRows
// documents out of it.
//
// The cursor's getMore round trips go back through the proxy like any other
// command, so paging is logged and governed too. Closing it sends killCursors,
// which is what stops the server holding a cursor open for rows nobody asked
// for after the cap.
func mongoRunCursor(ctx context.Context, database *mongo.Database, doc bson.D, maxRows int) (*QueryResult, error) {
	cursor, err := database.RunCommandCursor(ctx, doc)
	if err != nil {
		return nil, err
	}

	defer func() { _ = cursor.Close(context.WithoutCancel(ctx)) }()

	result := &QueryResult{}
	seen := make(map[string]struct{})

	for cursor.Next(ctx) {
		if len(result.Rows) >= maxRows {
			result.Truncated = true

			break
		}

		keys, values, decodeErr := mongoDocument(cursor.Current)
		if decodeErr != nil {
			return nil, decodeErr
		}

		// A MongoDB collection is not a table: documents in one result set may
		// carry different fields, so Columns is the union in first-seen order.
		for _, key := range keys {
			if _, dup := seen[key]; dup {
				continue
			}

			seen[key] = struct{}{}

			result.Columns = append(result.Columns, key)
		}

		result.Rows = append(result.Rows, values)
	}

	if !result.Truncated {
		if err := cursor.Err(); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// mongoRunCommand runs a command that answers with one reply document and
// reports that document as a single row.
func mongoRunCommand(ctx context.Context, database *mongo.Database, doc bson.D) (*QueryResult, error) {
	raw, err := database.RunCommand(ctx, doc).Raw()
	if err != nil {
		return nil, err
	}

	keys, values, err := mongoDocument(raw)
	if err != nil {
		return nil, err
	}

	result := &QueryResult{Columns: keys, Rows: []map[string]any{values}}

	// `n` is what insert/update/delete report, and it is the closest thing
	// MongoDB has to the rows-affected the other protocols return.
	if affected, ok := mongoInt64(values["n"]); ok {
		result.RowsAffected = &affected
	}

	return result, nil
}

// mongoDocument splits one BSON document into its keys, in wire order, and a
// JSON-friendly value map.
func mongoDocument(raw bson.Raw) ([]string, map[string]any, error) {
	elements, err := raw.Elements()
	if err != nil {
		return nil, nil, err
	}

	keys := make([]string, 0, len(elements))
	values := make(map[string]any, len(elements))

	for _, element := range elements {
		key, keyErr := element.KeyErr()
		if keyErr != nil {
			return nil, nil, keyErr
		}

		if _, dup := values[key]; !dup {
			keys = append(keys, key)
		}

		values[key] = normalizeBSONValue(element.Value())
	}

	return keys, values, nil
}

// normalizeBSONValue renders one BSON value as something json.Marshal turns
// into what an agent expects.
func normalizeBSONValue(value bson.RawValue) any {
	var decoded any

	if err := value.Unmarshal(&decoded); err != nil {
		// Anything the registry cannot hand back as a Go value is rendered as
		// its extended-JSON text rather than dropped.
		return value.String()
	}

	return jsonSafeBSON(decoded)
}

// jsonSafeBSON turns the driver's BSON-native types into JSON an agent can read.
//
// An ObjectID would otherwise marshal as a byte array and a DateTime as an
// epoch integer — both technically lossless and both unusable by a model that
// wants to quote the value back in the next statement.
func jsonSafeBSON(value any) any {
	switch v := value.(type) {
	case bson.D:
		out := make(map[string]any, len(v))
		for _, element := range v {
			out[element.Key] = jsonSafeBSON(element.Value)
		}

		return out
	case bson.A:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, jsonSafeBSON(item))
		}

		return out
	case bson.ObjectID:
		return v.Hex()
	case bson.DateTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	case bson.Decimal128:
		return v.String()
	case bson.Binary:
		return v.Data
	case bson.Timestamp:
		return fmt.Sprintf("%d:%d", v.T, v.I)
	case bson.Regex:
		return "/" + v.Pattern + "/" + v.Options
	default:
		return value
	}
}

// mongoInt64 reads a BSON number back as an int64.
func mongoInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		return int64(v), true
	default:
		return 0, false
	}
}

// mongoIntrospection renders the `describe` tool as a real MongoDB command.
//
// With no collection named it lists the database's collections. With one, it
// samples a single document: MongoDB has no column catalog, so the honest
// answer to "what are this collection's fields" is the shape of a document that
// is actually in it.
func mongoIntrospection(collection string) (string, []any, error) {
	if collection == "" {
		return mongoStatement("listCollections", bson.D{
			{Key: "listCollections", Value: 1},
			{Key: "nameOnly", Value: true},
		})
	}

	return mongoStatement("find", bson.D{
		{Key: "find", Value: collection},
		{Key: "limit", Value: 1},
	})
}

// mongoStatement renders a command document as the `<command> <extended JSON>`
// text the proxy logs and approval patterns match.
//
// The collection name is a *value* in a document handed to the BSON encoder,
// never text concatenated into JSON. That is the same rule the SQL protocols
// keep by binding table names: the input comes from a model, and this helper
// must not be the one place in dbbat that interpolates one.
func mongoStatement(command string, doc bson.D) (string, []any, error) {
	extJSON, err := bson.MarshalExtJSON(doc, false, false)
	if err != nil {
		return "", nil, fmt.Errorf("rendering the MongoDB command: %w", err)
	}

	return command + " " + string(extJSON), nil, nil
}

// renderSampledFields reports a MongoDB collection's shape from the sampled
// document `describe` fetched.
//
// It is explicitly not a schema: a heterogeneous collection has fields this
// does not show, and the types are the ones that document happened to use.
func renderSampledFields(result *QueryResult) []ColumnInfo {
	if result == nil {
		return nil
	}

	var first map[string]any
	if len(result.Rows) > 0 {
		first = result.Rows[0]
	}

	out := make([]ColumnInfo, 0, len(result.Columns))

	for _, name := range result.Columns {
		value := first[name]
		out = append(out, ColumnInfo{
			Name:     name,
			Type:     bsonTypeName(value),
			Nullable: value == nil,
		})
	}

	return out
}

// bsonTypeName names the BSON type a rendered value came from, well enough for
// an agent to plan a query against it.
func bsonTypeName(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case string:
		return "string"
	case int32, int64:
		return "int"
	case float64:
		return "double"
	case []byte:
		return "binData"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}
