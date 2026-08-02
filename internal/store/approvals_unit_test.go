package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestValidateApprovalPatterns(t *testing.T) {
	t.Parallel()

	if err := ValidateApprovalPatterns(nil); err != nil {
		t.Fatalf("empty pattern list rejected: %v", err)
	}

	if err := ValidateApprovalPatterns([]string{`(?i)^DELETE\s+FROM`, `(?i)\bGRANT\b`}); err != nil {
		t.Fatalf("valid patterns rejected: %v", err)
	}

	// A bad regexp must be a save-time error, not a runtime surprise on the
	// proxy hot path.
	if err := ValidateApprovalPatterns([]string{"(unclosed"}); err == nil {
		t.Fatal("an uncompilable pattern was accepted")
	}

	if err := ValidateApprovalPatterns([]string{""}); !errors.Is(err, ErrApprovalPatternEmpty) {
		t.Fatalf("got %v — a blank pattern matches everything and is never a policy", err)
	}

	long := strings.Repeat("a", MaxApprovalPatternLength+1)
	if err := ValidateApprovalPatterns([]string{long}); !errors.Is(err, ErrApprovalPatternTooLong) {
		t.Fatalf("got %v, want ErrApprovalPatternTooLong", err)
	}

	many := make([]string, MaxApprovalPatterns+1)
	for i := range many {
		many[i] = "x"
	}

	if err := ValidateApprovalPatterns(many); !errors.Is(err, ErrTooManyApprovalPatterns) {
		t.Fatalf("got %v, want ErrTooManyApprovalPatterns", err)
	}
}

func TestGrantRequiresApproval(t *testing.T) {
	t.Parallel()

	var nilGrant *AccessGrant
	if nilGrant.RequiresApproval() {
		t.Fatal("nil grant claims to require approval")
	}

	if (&AccessGrant{}).RequiresApproval() {
		t.Fatal("a grant with no patterns must not require approval")
	}

	if !(&AccessGrant{ApprovalPatterns: []string{"x"}}).RequiresApproval() {
		t.Fatal("a grant with a pattern must require approval")
	}
}

func TestGrantMayApprove(t *testing.T) {
	t.Parallel()

	sre := uuid.New()
	dba := uuid.New()
	other := uuid.New()

	grant := &AccessGrant{ApproverGroupUIDs: []uuid.UUID{sre, dba}}

	if !grant.MayApprove([]uuid.UUID{other, dba}) {
		t.Fatal("a member of an approver group was refused")
	}

	if grant.MayApprove([]uuid.UUID{other}) {
		t.Fatal("a non-member was accepted")
	}

	// Empty approver groups means admins only — never "everyone".
	if (&AccessGrant{}).MayApprove([]uuid.UUID{sre}) {
		t.Fatal("an empty approver-group list must not fail open")
	}

	var nilGrant *AccessGrant
	if nilGrant.MayApprove([]uuid.UUID{sre}) {
		t.Fatal("nil grant accepted an approver")
	}
}

func TestBuildGrantFromDefinitionMirrorsApprovalFields(t *testing.T) {
	t.Parallel()

	group := uuid.New()
	def := &GrantDefinition{
		DurationSeconds:   3600,
		ApprovalPatterns:  []string{`(?i)^DELETE`},
		ApproverGroupUIDs: []uuid.UUID{group},
	}

	grant := BuildGrantFromDefinition(def, uuid.New(), uuid.New(), uuid.New(), time.Now())

	if len(grant.ApprovalPatterns) != 1 || grant.ApprovalPatterns[0] != `(?i)^DELETE` {
		t.Fatalf("patterns not mirrored onto the grant: %v", grant.ApprovalPatterns)
	}

	if len(grant.ApproverGroupUIDs) != 1 || grant.ApproverGroupUIDs[0] != group {
		t.Fatalf("approver groups not mirrored: %v", grant.ApproverGroupUIDs)
	}

	// The copy must be independent: mutating the definition afterwards must
	// not reach a grant that was already materialized.
	def.ApprovalPatterns[0] = "changed"

	if grant.ApprovalPatterns[0] == "changed" {
		t.Fatal("the grant shares the definition's slice")
	}
}
