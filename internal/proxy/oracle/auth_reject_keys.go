package oracle

import (
	"errors"
	"fmt"
)

// keysWithoutVerifierError annotates an Oracle login failure with how many of
// the user's *live* API keys cannot serve an Oracle login at all — they carry no
// O5LOGON verifier (minted before Oracle support), or one that no longer
// decrypts under the current DBB_KEY.
//
// It exists because O5LOGON never reveals which key was typed. The proxy hands
// the client a challenge built from the user's shared salts and learns only that
// no candidate decrypted AUTH_PASSWORD; the key actually presented is, in this
// exact failure, not among the candidates at all — that is what makes it
// invisible. So dbbat cannot say "the key you used predates Oracle support". It
// can say the strictly weaker, still-actionable thing: none of your usable keys
// matched, and N of your keys are not usable here.
//
// The wrapped error is preserved, so `errors.Is(err, ErrAPIKeyVerification)`
// keeps working for every caller that classifies the failure.
type keysWithoutVerifierError struct {
	err   error
	count int
}

func (e *keysWithoutVerifierError) Error() string {
	return fmt.Sprintf("%s (%d of the user's API keys have no usable O5LOGON verifier)", e.err, e.count)
}

func (e *keysWithoutVerifierError) Unwrap() error { return e.err }

// noteKeysWithoutVerifier wraps a failed Oracle login with the count above, so
// authRejectFor can turn it into a message the user can act on.
//
// Only the two failures the count actually explains are annotated: no candidate
// decrypted the password (ErrAPIKeyVerification) and the user has no candidate
// at all (ErrNoO5LogonVerifier). A store error or a parse failure is not made
// clearer by an inventory of keys, and must not borrow this message.
func noteKeysWithoutVerifier(err error, count int) error {
	if count <= 0 {
		return err
	}

	if !errors.Is(err, ErrAPIKeyVerification) && !errors.Is(err, ErrNoO5LogonVerifier) {
		return err
	}

	return &keysWithoutVerifierError{err: err, count: count}
}

// keysWithoutVerifierMessage is the sentence a client is shown when its login
// failed and the user owns keys that could never have worked.
//
// It is deliberately counts-and-advice only: no key prefix, no key name, nothing
// derived from the material. It is also deliberately hedged — dbbat does not
// know that the key just typed was one of the unusable ones, only that some
// exist — so the primary statement stays "invalid username/password" and the
// note follows it.
//
// It is pre-auth disclosure, which the codebase already does on the same frame:
// ErrNoActiveGrant answers a client that has not authenticated with "no active
// grant for this database", naming an account-shaped fact. This adds a count of
// that user's Oracle-incapable keys, reachable only by someone who already knows
// the username and already failed a login with it, and it names nothing secret.
// ASCII only: the CLR is rendered by clients under charsets dbbat does not pick.
func keysWithoutVerifierMessage(count int) string {
	return fmt.Sprintf("invalid username/password; %d of your dbbat API keys predate Oracle "+
		"support and cannot be used for Oracle login; mint a new key", count)
}
