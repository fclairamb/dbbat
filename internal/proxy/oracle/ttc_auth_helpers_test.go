package oracle

import "testing"

// TestIdentifierRunVsPrintableASCIIRun locks in the acceptance sets of the two
// easily-confused byte-classification helpers so the isPrintableASCII misnomer
// (which truncated dotted usernames, see PR #235) can never silently return.
//
//   - isIdentifierRun     — Oracle identifier bytes only: A-Z a-z 0-9 _ $ #
//   - isPrintableASCIIRun — any printable ASCII byte, 0x20–0x7E (dots, dashes,
//     at-signs and spaces included); rejects empty input.
func TestIdentifierRunVsPrintableASCIIRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      []byte
		identifier bool // expected isIdentifierRun
		printable  bool // expected isPrintableASCIIRun
	}{
		{"plain identifier", []byte("ADMIN"), true, true},
		{"identifier with digits", []byte("user01"), true, true},
		{"identifier symbols _ $ #", []byte("a_b$c#d"), true, true},
		{"dotted username", []byte("florent.clairambault"), false, true},
		{"dash", []byte("read-only"), false, true},
		{"at sign", []byte("user@host"), false, true},
		{"space", []byte("a b"), false, true},
		{"password punctuation", []byte("P@ss.w0rd!"), false, true},
		{"tilde (top of printable range 0x7e)", []byte("~"), false, true},
		{"space alone (bottom of printable range 0x20)", []byte(" "), false, true},
		{"del (0x7f, non-printable)", []byte{0x7f}, false, false},
		{"control byte (0x1f)", []byte{0x1f}, false, false},
		{"null byte", []byte{0x00}, false, false},
		{"high byte (0x80)", []byte{0x80}, false, false},
		{"empty", []byte{}, true, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := isIdentifierRun(tc.input); got != tc.identifier {
				t.Errorf("isIdentifierRun(%q) = %v, want %v", tc.input, got, tc.identifier)
			}

			if got := isPrintableASCIIRun(tc.input); got != tc.printable {
				t.Errorf("isPrintableASCIIRun(%q) = %v, want %v", tc.input, got, tc.printable)
			}
		})
	}
}
