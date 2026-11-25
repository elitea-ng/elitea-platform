package adminaccess_test

import (
	"errors"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess/rolegrants"
)

// rolegrants cannot import adminaccess (adminaccess's writers import it), so
// it holds its own copy of the reserved address; the two must never drift.
func TestTheRoleWriterRefusesTheSameReservedAddress(t *testing.T) {
	if rolegrants.ReservedAccountEmail != adminaccess.ReservedBreakGlassEmail {
		t.Fatalf("rolegrants refuses %q, the reserved account is %q",
			rolegrants.ReservedAccountEmail, adminaccess.ReservedBreakGlassEmail)
	}
}

func TestIsReservedBreakGlassEmail(t *testing.T) {
	for email, want := range map[string]bool{
		"break-glass@centry.user":     true,
		" Break-Glass@Centry.USER ":   true,
		"break-glass@centry.user.com": false,
		"break-glass@example.com":     false,
		"":                            false,
	} {
		if got := adminaccess.IsReservedBreakGlassEmail(email); got != want {
			t.Errorf("%q: %v, want %v", email, got, want)
		}
	}
}

// An unbound issue names no person, and needs a ticket and a custodian label;
// a ticket is an identifier, never free text.
func TestSealedIssueValidateUnbound(t *testing.T) {
	valid := adminaccess.SealedIssue{Unbound: true, Ticket: "INC-4711", Label: "envelope A", At: time.Now()}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid unbound issue: %v", err)
	}
	for name, issue := range map[string]adminaccess.SealedIssue{
		"with a person":    {Unbound: true, UserID: 7, Ticket: "INC-1", Label: "A"},
		"no ticket":        {Unbound: true, Label: "A"},
		"no label":         {Unbound: true, Ticket: "INC-1"},
		"free-text ticket": {Unbound: true, Ticket: "outage on monday", Label: "A"},
		"bound, bad tick":  {UserID: 7, Ticket: "a\nb"},
		"bound, no person": {Ticket: "INC-1"},
	} {
		issue.At = valid.At
		if err := issue.Validate(); !errors.Is(err, adminaccess.ErrSealedCredentialInvalid) {
			t.Errorf("%s: %v, want ErrSealedCredentialInvalid", name, err)
		}
	}
}
