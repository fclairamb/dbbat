package mongodb

import (
	"strconv"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/store"
)

// cursorOrigin records the command (and namespace) that opened a server cursor,
// so a later getMore can be linked back to it (item 6).
type cursorOrigin struct {
	command   string
	namespace string
}

// cursorOpeningCommands are the reads whose reply may open a server cursor that
// a subsequent getMore iterates.
var cursorOpeningCommands = map[string]bool{
	"find": true, "aggregate": true, "listCollections": true, "listIndexes": true,
}

// putCursorOrigin records the origin of a newly opened cursor.
func (s *Session) putCursorOrigin(id int64, origin cursorOrigin) {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()

	s.cursorOrigins[id] = origin
}

// getCursorOrigin returns the recorded origin for a cursor id, if known.
func (s *Session) getCursorOrigin(id int64) (cursorOrigin, bool) {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()

	origin, ok := s.cursorOrigins[id]

	return origin, ok
}

// dropCursorOrigin forgets a cursor once it is exhausted or killed.
func (s *Session) dropCursorOrigin(id int64) {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()

	delete(s.cursorOrigins, id)
}

// annotateGetMore links a getMore command to the find/aggregate cursor it
// iterates (item 6): it stamps the pending query with the shared cursor_id and,
// when known, the originating command + namespace, so a viewer can read a whole
// paged result set as one logical query. It also records the cursor id so the
// origin link can be dropped when the cursor drains.
func (s *Session) annotateGetMore(pq *pendingQuery, body bson.Raw) {
	cursorID, ok := lookupInt64(body, "getMore")
	if !ok {
		return
	}

	pq.cursorID = cursorID
	appendPendingParam(pq, "cursor_id="+strconv.FormatInt(cursorID, 10))

	if origin, ok := s.getCursorOrigin(cursorID); ok {
		ref := origin.command
		if origin.namespace != "" {
			ref += " " + origin.namespace
		}

		appendPendingParam(pq, "cursor_origin="+ref)
	}
}

// trackCursorFromReply maintains the cursor→origin map from an upstream reply
// and stamps a cursor-opening command's own log with its cursor_id, so the
// origin and every getMore batch share one cursor_id (item 6).
func (s *Session) trackCursorFromReply(pq *pendingQuery, body bson.Raw) {
	cursor, ok := body.Lookup("cursor").DocumentOK()
	if !ok {
		return
	}

	replyCursorID, _ := lookupInt64(cursor, "id")

	switch {
	case cursorOpeningCommands[pq.command]:
		if replyCursorID != 0 {
			s.putCursorOrigin(replyCursorID, cursorOrigin{
				command:   pq.command,
				namespace: lookupString(cursor, "ns"),
			})
			appendPendingParam(pq, "cursor_id="+strconv.FormatInt(replyCursorID, 10))
		}
	case pq.command == "getMore":
		// A drained cursor reports id 0; forget its origin link.
		if replyCursorID == 0 && pq.cursorID != 0 {
			s.dropCursorOrigin(pq.cursorID)
		}
	}
}

// appendPendingParam appends a "key=value" annotation to a pending query's
// logged parameters, allocating the parameter set if needed.
func appendPendingParam(pq *pendingQuery, value string) {
	if pq.params == nil {
		pq.params = &store.QueryParameters{}
	}

	pq.params.Values = append(pq.params.Values, value)
}
