package shared

import (
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// A dbbat session logs in to the target as one shared database role, from one
// host — the proxy. Every tool that reads the *target's* own instrumentation
// therefore attributes the whole fleet's load to that one role: RDS
// Performance Insights groups by SQL digest and shows `prod_datalake` from the
// proxy's IP, `pg_stat_statements` keys on `(userid, dbid, queryid)` with no
// application_name dimension at all, and the slow query log prints statement
// text and nothing else.
//
// What all three *do* show is the statement text, which is why Google's
// sqlcommenter convention puts the identity there:
//
//	/*dbbat='0.28.1',user='florent',conn='3f9a1c7b2e4d',grant='diag-paris'*/ SELECT …
//
// This file builds that comment. Two properties are load-bearing:
//
//   - **It is prepended, not appended.** sqlcommenter appends, but
//     `pg_stat_activity.query` truncates at `track_activity_query_size` (1024
//     bytes by default) and the slow log truncates too — so on exactly the long
//     statements a DBA is hunting, an appended tag is the part that gets cut.
//   - **It is a pure function of (version, user, connection, grant).** No
//     timestamp, no per-statement counter, nothing that varies between two
//     executions of the same statement on the same session. That is what keeps
//     `pg_stat_statements` and the MySQL digest aggregating repeated executions
//     into one row instead of one row per execution.
//
// Nothing here ever reaches the store: the tag is applied to the bytes going
// upstream and to nothing else, so `queries`, the audit chain and the UI's
// query-text search all keep the client's original statement. It is
// reconstructible from the connection row anyway, which is why storing it would
// buy nothing.

// queryTagKeys are the tag's keys, in the order they are emitted. The order is
// fixed rather than sorted-at-build-time so the bytes are stable: a tag whose
// key order could vary between two runs would split one `pg_stat_statements`
// digest into several.
const (
	queryTagKeyVersion = "dbbat"
	queryTagKeyUser    = "user"
	queryTagKeyConn    = "conn"
	queryTagKeyGrant   = "grant"
)

// QueryTagger prepends the dbbat identity comment to statements on their way
// upstream.
//
// The zero value is **inert**: Apply returns its argument untouched. That is
// what a session gets when DBB_QUERY_TAGGING is off, so the disabled path costs
// one string comparison and cannot change a single byte on the wire.
type QueryTagger struct {
	// fields is the bare `key='value',…` body, in the fixed key order above,
	// without the comment delimiters. It is what a protocol that has no SQL
	// comments carries instead — MongoDB puts it in the command's `comment`
	// field — so the same bytes identify a session whatever the wire format.
	// Empty = inert.
	fields string
	// prefix is the whole comment plus its trailing space, precomputed once
	// per session. Empty = inert.
	prefix string
}

// NewQueryTagger builds the tagger for one session. Empty components are
// omitted rather than emitted as an empty-valued key: a shapeless grant or an
// unidentified connection should shorten the tag, not pad it with noise that
// reads like a value.
//
// connUID is the same uuid the upstream application/program name carries, and
// the `conn=` value is the same 12 hex characters BuildUpstreamName's `c=`
// field uses — deliberately, so `pg_stat_activity.query` and
// `pg_stat_activity.application_name` name the same dbbat connection, findable
// with GET /api/v1/connections?uid_suffix=.
//
// Every value is percent-encoded (Go's url.QueryEscape, which is the encoding
// the sqlcommenter spec calls for). The username and the grant slug are
// already slug-safe, so in practice this is a no-op — but it is what makes the
// tag *structurally* safe rather than safe by convention: `'` becomes `%27` and
// `*` becomes `%2A`, so no value can close the comment early, terminate the
// quoting, or otherwise inject SQL into the statement being forwarded.
func NewQueryTagger(dbbatVersion, username string, connUID uuid.UUID, grantSlug string) QueryTagger {
	var b strings.Builder

	appendQueryTagField(&b, queryTagKeyVersion, dbbatVersion)
	appendQueryTagField(&b, queryTagKeyUser, username)
	appendQueryTagField(&b, queryTagKeyConn, uidSuffix(connUID))
	appendQueryTagField(&b, queryTagKeyGrant, grantSlug)

	if b.Len() == 0 {
		return QueryTagger{}
	}

	fields := b.String()

	return QueryTagger{fields: fields, prefix: "/*" + fields + "*/ "}
}

// appendQueryTagField writes `,key='value'` (without the leading comma for the
// first field) when value is non-empty, and nothing at all when it is.
func appendQueryTagField(b *strings.Builder, key, value string) {
	if value == "" {
		return
	}

	if b.Len() > 0 {
		b.WriteByte(',')
	}

	b.WriteString(key)
	b.WriteString("='")
	b.WriteString(url.QueryEscape(value))
	b.WriteByte('\'')
}

// Active reports whether this tagger will actually change anything.
func (t QueryTagger) Active() bool {
	return t.prefix != ""
}

// Tag is the identity itself — `dbbat='…',user='…',conn='…',grant='…'` — with
// no comment delimiters and no trailing space, or "" for an inert tagger.
//
// It exists for MongoDB, which has no statement text to prepend a comment to
// but does have a first-class `comment` command field that the profiler and
// db.currentOp() echo back. Handing that field this exact string (rather than
// a BSON sub-document of the same four values) is deliberate: it is what
// sqlcommenter consumers already parse, it greps identically to the comment
// the SQL proxies emit, and it renders inline in the Atlas profiler instead of
// as a collapsed sub-document.
//
// Same guarantees as Prefix: fixed key order, percent-encoded values, no
// timestamp and no per-statement id.
func (t QueryTagger) Tag() string {
	return t.fields
}

// Prefix is the comment (plus its single trailing space) Apply prepends, or ""
// for an inert tagger. Exported for the tests and for anything that needs to
// assert on the exact bytes.
func (t QueryTagger) Prefix() string {
	return t.prefix
}

// Apply returns sql with the tag prepended.
//
// A blank statement is returned untouched: PostgreSQL answers an empty query
// string with EmptyQueryResponse, and turning it into a comment-only statement
// — which is *also* an empty query, but by a different route — is a change in
// the bytes on the wire for no observability gain at all.
func (t QueryTagger) Apply(sql string) string {
	if t.prefix == "" || strings.TrimSpace(sql) == "" {
		return sql
	}

	return t.prefix + sql
}
