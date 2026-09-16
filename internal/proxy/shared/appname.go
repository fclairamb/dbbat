package shared

import (
	"strings"

	"github.com/google/uuid"
)

// uidTagLen is the number of hex characters from the tail of the connection
// uuid embedded in the upstream name's "c=" tag. For a UUIDv7 the leading
// characters are a millisecond timestamp — shared by every connection opened
// in the same instant — so the tag takes the trailing, random portion
// instead. 12 hex characters is 48 bits, enough to be unique among every
// connection an instance will ever see.
const uidTagLen = 12

// BuildUpstreamName composes the canonical dbbat-branded application/program
// name sent to upstream databases, so a DBA looking at the target's session
// views (pg_stat_activity.application_name, V$SESSION.PROGRAM, MySQL's
// process list) can attribute a session not just to the dbbat user who
// initiated it, but to the exact dbbat connection — the queries, the grant,
// the Terminate button — via GET /api/v1/connections?uid_suffix=.
//
// Format:
//
//	dbbat/$version @$username
//
// plus, when connUID is non-nil, the last uidTagLen hex characters of its
// canonical (dashless) form:
//
//	dbbat/$version @$username c=$uidSuffix
//
// and, when the client declared an application/program name dbbat was able
// to intercept:
//
//	dbbat/$version @$username c=$uidSuffix for $appName
//
// The result is truncated to fit maxLen. Truncation order: $appName is cut
// first (partially, if that still leaves room for some of it), then
// $version; "@$username" and "c=$uidSuffix" are never cut — the tag is what
// makes a pg_stat_activity row actionable, so it survives even when a long
// username leaves little other room (Oracle's 48-byte cap in particular).
// maxLen <= 0 is treated as "no room at all" and returns "".
func BuildUpstreamName(dbbatVersion, username string, connUID uuid.UUID, clientAppName string, maxLen int) string {
	userPart := "@" + username
	if suffix := uidSuffix(connUID); suffix != "" {
		userPart += " c=" + suffix
	}

	clientAppName = strings.TrimSpace(clientAppName)

	if full := compose(dbbatVersion, userPart, clientAppName); len(full) <= maxLen {
		return full
	}

	base := compose(dbbatVersion, userPart, "")

	if clientAppName != "" {
		const sep = " for "
		if avail := maxLen - len(base) - len(sep); avail > 0 {
			return base + sep + clientAppName[:avail]
		}
		// No room for any app name at all: fall through and shrink the
		// version instead, exactly as if none had been supplied.
	}

	if len(base) <= maxLen {
		return base
	}

	return shrinkVersion(dbbatVersion, userPart, maxLen)
}

// compose assembles "dbbat/$version $userPart", plus " for $clientAppName"
// when clientAppName is non-empty.
func compose(dbbatVersion, userPart, clientAppName string) string {
	s := "dbbat/" + dbbatVersion + " " + userPart
	if clientAppName != "" {
		s += " for " + clientAppName
	}

	return s
}

// shrinkVersion truncates dbbatVersion to make "dbbat/$version $userPart" fit
// maxLen, preferring to cut the version over userPart's "@user c=..." — the
// part of the name that turns a pg_stat_activity row back into a dbbat
// connection. If even a zero-length version doesn't fit, userPart itself is
// truncated as an absolute last resort; every protocol dbbat proxies to sets
// a maxLen well above that floor, so this only fires under an adversarial
// (e.g. test-only) cap.
func shrinkVersion(dbbatVersion, userPart string, maxLen int) string {
	const prefix = "dbbat/"

	skeleton := prefix + " " + userPart // the version-less form

	avail := maxLen - len(skeleton)
	if avail <= 0 {
		return truncateName(skeleton, maxLen)
	}

	version := dbbatVersion
	if len(version) > avail {
		version = version[:avail]
	}

	return prefix + version + " " + userPart
}

// uidSuffix returns the last uidTagLen hex characters of connUID's canonical
// (dashless) form — for a UUIDv7 that's exactly its final, purely-random
// dash-delimited group — or "" for the zero UUID, which is what every caller
// with no connection uid yet (the connectivity probe) passes.
func uidSuffix(connUID uuid.UUID) string {
	if connUID == uuid.Nil {
		return ""
	}

	hex := strings.ReplaceAll(connUID.String(), "-", "")
	if len(hex) <= uidTagLen {
		return hex
	}

	return hex[len(hex)-uidTagLen:]
}

// truncateName truncates s to at most maxLen bytes. maxLen <= 0 yields "".
func truncateName(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}

	if len(s) <= maxLen {
		return s
	}

	return s[:maxLen]
}
