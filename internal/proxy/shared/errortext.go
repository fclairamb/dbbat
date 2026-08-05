package shared

import (
	"context"
	"log/slog"
	"unicode/utf8"
)

// SanitizeQueryError is the last gate before a query error string reaches the
// store. It returns queryError unchanged when the text is something a human
// could read, and nil when it is not.
//
// Query.Error is a human-facing text column: everything legitimately written to
// it is a server diagnostic or a dbbat message, i.e. printable text. A value
// that is not valid UTF-8, or that carries control bytes, can only have come
// from a decoder misreading wire bytes — most famously the Oracle legacy
// fixed-offset Response layout, which read column-compressed row data as an
// error code plus a length-prefixed message. Dropping such a value is strictly
// better than persisting binary noise that then reads as a failed query in the
// UI and the API.
//
// The drop is logged at debug with the offending length so the misparse is
// still traceable, without spilling the bytes into the logs.
func SanitizeQueryError(ctx context.Context, logger *slog.Logger, queryError *string) *string {
	if queryError == nil {
		return nil
	}

	if isPrintableErrorText(*queryError) {
		return queryError
	}

	if logger != nil {
		logger.DebugContext(ctx, "dropping non-diagnostic query error text",
			slog.Int("length", len(*queryError)))
	}

	return nil
}

// isPrintableErrorText reports whether s is valid UTF-8 free of control
// characters. Tab, newline and carriage return are allowed: multi-line
// diagnostics (Oracle error stacks, PostgreSQL DETAIL/HINT lines) use them.
func isPrintableErrorText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}

	for _, r := range s {
		if r == '\t' || r == '\n' || r == '\r' {
			continue
		}

		// C0 controls, DEL, and the C1 control block: never part of a
		// diagnostic, always a sign of misread binary.
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return false
		}
	}

	return true
}
