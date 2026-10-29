package pipelinetriggers

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// An admission refused under its mandate is named by its cause: a stale
// mandate is MANDATE_NOT_USABLE, a rollback to a release the mandate does
// not bind is MANDATE_RELEASE_ROLLED_BACK with advice the settings tab
// shows. Any other error is not a mandate refusal.
func TestGovernedStartRefusalNamesTheRollback(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{fmt.Errorf("admit: %w", repos.ErrRunAuthorityNotUsable), runmandate.CodeMandateNotUsable},
		{fmt.Errorf("admit: %w", repos.ErrRunAuthorityReleaseRolledBack), runmandate.CodeMandateReleaseRolledBack},
		{errors.New("database down"), ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := governedStartRefusal(c.err); got != c.code {
			t.Errorf("governedStartRefusal(%v) = %q, want %q", c.err, got, c.code)
		}
	}
	result, detail := mandateSkip(runmandate.CodeMandateReleaseRolledBack)
	if result != resultSkippedMandateUnusable || !strings.HasPrefix(detail, runmandate.CodeMandateReleaseRolledBack+": ") ||
		!strings.Contains(detail, "rolled back") {
		t.Fatalf("mandateSkip = %q, %q", result, detail)
	}
}
