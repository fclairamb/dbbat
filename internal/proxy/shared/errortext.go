package shared

import (
	"context"
	"log/slog"
	"strings"
	"unicode/utf8"
)

// maxUndecodableShare bounds how much of an error string may fail to decode as
// UTF-8 before the whole thing is dropped instead of repaired.
//
// A diagnostic emitted in a single-byte charset (WE8ISO8859P1 and friends — the
// common case on European Oracle estates) has a handful of undecodable bytes in
// an otherwise ASCII sentence: "ORA-00001: contrainte unique violée" is one bad
// byte in thirty-four. Misread binary is the opposite shape. A quarter sits well
// clear of both.
const maxUndecodableShare = 4 // more than 1/maxUndecodableShare undecodable → drop

// SanitizeQueryError is the last gate before a query error string reaches the
// store. It returns text a human could read, or nil when the value cannot be
// salvaged.
//
// Query.Error is a human-facing column: everything legitimately written to it
// is a server diagnostic or a dbbat message. Two things can go wrong:
//
//   - Control bytes. No diagnostic contains them; they only appear when a
//     decoder misreads wire bytes — as the Oracle legacy fixed-offset Response
//     layout did, copying column-compressed row data (0x15 descriptors, 0x07
//     separators) into the error field. Such a value is dropped outright.
//   - Undecodable bytes. dbbat does not know the session charset, so a genuine
//     diagnostic from a non-AL32UTF8 session is not valid UTF-8. Dropping those
//     would silently lose real errors, so a mostly-decodable string is repaired
//     instead: the bad bytes become U+FFFD and the text is kept. A mangled
//     "contrainte unique viol<?>e" tells an operator what happened; nothing at
//     all does not. Past maxUndecodableShare the string is not a sentence with
//     accents in it — it is binary — and is dropped.
//
// Both outcomes are logged at debug with the length only, never the bytes, so
// the misparse stays traceable without spilling row data into the logs.
func SanitizeQueryError(ctx context.Context, logger *slog.Logger, queryError *string) *string {
	if queryError == nil {
		return nil
	}

	cleaned, ok := sanitizeErrorText(*queryError)
	if !ok {
		debugLogErrorText(ctx, logger, "dropping non-diagnostic query error text", len(*queryError))

		return nil
	}

	if cleaned != *queryError {
		debugLogErrorText(ctx, logger,
			"query error text had undecodable bytes; kept with replacements", len(*queryError))
	}

	return &cleaned
}

// SanitizeStatementText applies the same judgement to a candidate *statement*
// run, and exists so the reasoning and the threshold live in one place rather
// than being reinvented per protocol.
//
// The Oracle execute decoder needs exactly this question answered: a run of
// bytes is either the statement the header declared or it is misread binary,
// and "is it valid UTF-8" is the wrong test for the same reason it is the wrong
// test for a diagnostic — dbbat does not know the session charset, so a
// perfectly ordinary `INSERT INTO t VALUES ('café')` from a WE8ISO8859P1
// session is not valid UTF-8. Refusing it there does not merely lose fidelity:
// the decoder falls back to a keyword scan that truncates the statement at the
// first accented byte, so a blocked pattern or an approval pattern in the tail
// stops matching.
//
// The repaired text is what callers store, and that matters beyond
// readability: `queries.sql_text` is a Postgres `text` column, so a run kept
// verbatim would fail the insert and take the audit row with it.
func SanitizeStatementText(s string) (string, bool) {
	return sanitizeErrorText(s)
}

func debugLogErrorText(ctx context.Context, logger *slog.Logger, msg string, length int) {
	if logger == nil {
		return
	}

	logger.DebugContext(ctx, msg, slog.Int("length", length))
}

// sanitizeErrorText reports whether s is usable as a query error and returns
// the text to store. Undecodable bytes are replaced with U+FFFD; ok is false
// when s carries a control byte or is too undecodable to be a sentence.
//
// Tab, newline and carriage return are allowed: multi-line diagnostics (Oracle
// error stacks, PostgreSQL DETAIL/HINT lines) use them.
func sanitizeErrorText(s string) (string, bool) {
	var (
		out         strings.Builder
		runes       int
		undecodable int
	)

	out.Grow(len(s))

	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		runes++

		// DecodeRuneInString reports RuneError with size 1 for a byte that is
		// not part of a valid sequence (a real U+FFFD decodes with size 3).
		if r == utf8.RuneError && size == 1 {
			undecodable++

			out.WriteRune(utf8.RuneError)

			i++

			continue
		}

		if !isAllowedErrorRune(r) {
			return "", false
		}

		out.WriteRune(r)

		i += size
	}

	if undecodable*maxUndecodableShare > runes {
		return "", false
	}

	return out.String(), true
}

// isAllowedErrorRune rejects the C0 controls, DEL and the C1 block, which never
// occur in a diagnostic and always signal misread binary.
func isAllowedErrorRune(r rune) bool {
	if r == '\t' || r == '\n' || r == '\r' {
		return true
	}

	return r >= 0x20 && r != 0x7f && (r < 0x80 || r > 0x9f)
}
