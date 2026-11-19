package shared

import "unicode/utf8"

// TruncateOnRune returns s cut to at most maxLen bytes, on a rune boundary.
//
// Statement text is bounded in bytes before it is stored, but it is UTF-8: a
// blind s[:maxLen] can land inside a multi-byte character, and the invalid
// string that results fails the `queries` insert (`sql_text` is a Postgres
// `text` column), so the statement — which has already run upstream — would
// vanish from the query history. A client that pads a statement so a
// multi-byte character straddles the limit could hide any statement that way.
func TruncateOnRune(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}

	if len(s) <= maxLen {
		return s
	}

	cut := maxLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut]
}
