package shared

import "strings"

// BuildUpstreamName composes the canonical dbbat-branded application/program
// name sent to upstream databases, so a DBA looking at the target's session
// views (pg_stat_activity.application_name, V$SESSION.PROGRAM, MySQL's
// process list) can attribute a session to the dbbat user who initiated it.
//
// Format:
//
//	dbbat/$version @$username
//
// and, when the client declared an application/program name dbbat was able
// to intercept:
//
//	dbbat/$version @$username for $appName
//
// The result is truncated to fit maxLen, preferring to truncate $appName
// first so the "dbbat/$version @$username" prefix survives intact. If even
// the bare prefix exceeds maxLen, the prefix itself is truncated as a last
// resort. maxLen <= 0 is treated as "no room at all" and returns "".
func BuildUpstreamName(dbbatVersion, username, clientAppName string, maxLen int) string {
	base := "dbbat/" + dbbatVersion + " @" + username

	clientAppName = strings.TrimSpace(clientAppName)
	if clientAppName == "" {
		return truncateName(base, maxLen)
	}

	const sep = " for "

	full := base + sep + clientAppName
	if len(full) <= maxLen {
		return full
	}

	avail := maxLen - len(base) - len(sep)
	if avail <= 0 {
		return truncateName(base, maxLen)
	}

	return base + sep + clientAppName[:avail]
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
