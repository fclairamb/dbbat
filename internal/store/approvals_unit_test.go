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

	withPatterns := &AccessGrant{Definition: &GrantDefinition{ApprovalPatterns: []string{"x"}}}
	if !withPatterns.RequiresApproval() {
		t.Fatal("a grant with a pattern must require approval")
	}
}

func TestGrantMayApprove(t *testing.T) {
	t.Parallel()

	sre := uuid.New()
	dba := uuid.New()
	other := uuid.New()

	grant := &AccessGrant{Definition: &GrantDefinition{ApproverUserGroupUIDs: []uuid.UUID{sre, dba}}}

	if !grant.MayApprove([]uuid.UUID{other, dba}) {
		t.Fatal("a member of an approver group was refused")
	}

	if grant.MayApprove([]uuid.UUID{other}) {
		t.Fatal("a non-member was accepted")
	}

	// Empty approver groups means admins only — never "everyone".
	if (&AccessGrant{Definition: &GrantDefinition{}}).MayApprove([]uuid.UUID{sre}) {
		t.Fatal("an empty approver-group list must not fail open")
	}

	// A grant whose definition never got attached is the fail-closed case: it
	// must not hand approval rights to anybody.
	if (&AccessGrant{}).MayApprove([]uuid.UUID{sre}) {
		t.Fatal("a grant with no definition attached must not fail open")
	}

	var nilGrant *AccessGrant
	if nilGrant.MayApprove([]uuid.UUID{sre}) {
		t.Fatal("nil grant accepted an approver")
	}
}

func TestBuildGrantFromDefinitionReadsApprovalFieldsFromTheDefinition(t *testing.T) {
	t.Parallel()

	group := uuid.New()
	defUID := uuid.New()
	def := &GrantDefinition{
		UID:               defUID,
		DurationSeconds:   3600,
		ApprovalPatterns:  []string{`(?i)^DELETE`},
		ApproverUserGroupUIDs: []uuid.UUID{group},
	}

	grant := BuildGrantFromDefinition(def, uuid.New(), uuid.New(), uuid.New(), time.Now())

	if grant.GrantDefinitionID != defUID {
		t.Fatalf("grant pins %s, want the definition %s", grant.GrantDefinitionID, defUID)
	}

	if len(grant.ApprovalPatterns()) != 1 || grant.ApprovalPatterns()[0] != `(?i)^DELETE` {
		t.Fatalf("patterns not read from the definition: %v", grant.ApprovalPatterns())
	}

	if len(grant.ApproverUserGroupUIDs()) != 1 || grant.ApproverUserGroupUIDs()[0] != group {
		t.Fatalf("approver groups not read from the definition: %v", grant.ApproverUserGroupUIDs())
	}
}

// A grant whose definition could not be attached must behave as the most
// restrictive grant imaginable rather than as an unrestricted one — the whole
// reason the accessors exist rather than plain field reads.
func TestGrantWithoutDefinitionFailsClosed(t *testing.T) {
	t.Parallel()

	grant := &AccessGrant{}

	for _, control := range ValidControls {
		if !grant.HasControl(control) {
			t.Errorf("a shapeless grant must report %s as enforced", control)
		}
	}

	maxQueries := grant.MaxQueryCounts()
	if maxQueries == nil || *maxQueries != 0 {
		t.Errorf("MaxQueryCounts() = %v, want an exhausted quota", maxQueries)
	}

	maxBytes := grant.MaxBytesTransferred()
	if maxBytes == nil || *maxBytes != 0 {
		t.Errorf("MaxBytesTransferred() = %v, want an exhausted quota", maxBytes)
	}
}
