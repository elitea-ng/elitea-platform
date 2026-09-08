package toolkitcatalogue

// The resolution order, measured in both directions.
//
// A filter that allows everything passes any positive-only suite, and so does
// one that refuses everything. Every case below therefore asserts the decision
// AND the layer that made it, so a filter that reached the right answer through
// the wrong rule fails.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func policy(toolkitType string, availability Availability) Policy {
	return Policy{
		ToolkitType:  toolkitType,
		Availability: availability,
		Reason:       "a recorded reason",
		DecidedBy:    "operator@example.com",
	}
}

func grant(toolkitType string, projectID int64, availability Availability) Grant {
	return Grant{
		ToolkitType:  toolkitType,
		ProjectID:    projectID,
		Availability: availability,
		Reason:       "a recorded grant reason",
		GrantedBy:    "operator@example.com",
	}
}

// THE HEADLINE RULE. A deployment that never opened the admin page serves every
// type. An empty table read as "everything is off" is the failure this package
// exists to prevent, and it is the one a positive-only suite cannot see.
func TestNoRowsServesEveryType(t *testing.T) {
	t.Parallel()

	filter := BuildFilter(7, nil, nil)

	require.True(t, filter.Allows("github"))
	require.True(t, filter.Allows("a type nobody has ever heard of"))
	require.Equal(t, 0, filter.Len())
	require.Equal(t, SourceDefault, filter.Decision("github").Source)
	require.Empty(t, filter.Decision("github").Reason,
		"a type nobody decided about must carry no invented reason")
}

// EmptyFilter is what an unwired or unreadable source resolves to, so it must
// behave exactly like a deployment with no rows.
func TestEmptyFilterAllowsEverything(t *testing.T) {
	t.Parallel()

	require.True(t, EmptyFilter().Allows("github"))
	require.True(t, EmptyFilter().Allows(""))
	require.Equal(t, 0, EmptyFilter().Len())
}

func TestResolutionOrder(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		policies    []Policy
		grants      []Grant
		wantAllowed bool
		wantSource  Source
	}{
		"enabled with no grant is served": {
			policies:    []Policy{policy("github", AvailabilityEnabled)},
			wantAllowed: true,
			wantSource:  SourceDeployment,
		},
		"disabled with no grant is withheld": {
			policies:    []Policy{policy("github", AvailabilityDisabled)},
			wantAllowed: false,
			wantSource:  SourceDeployment,
		},
		"restricted with no grant is withheld": {
			policies:    []Policy{policy("github", AvailabilityRestricted)},
			wantAllowed: false,
			wantSource:  SourceDeployment,
		},
		"restricted with an enabled grant is served to that project": {
			policies:    []Policy{policy("github", AvailabilityRestricted)},
			grants:      []Grant{grant("github", 7, AvailabilityEnabled)},
			wantAllowed: true,
			wantSource:  SourceProject,
		},
		"enabled with a disabled grant is withheld from that project": {
			policies:    []Policy{policy("github", AvailabilityEnabled)},
			grants:      []Grant{grant("github", 7, AvailabilityDisabled)},
			wantAllowed: false,
			wantSource:  SourceProject,
		},
		// The rule the header argues for: a refusal a stale grant could survive
		// is a refusal an operator cannot trust.
		"disabled beats an enabled grant": {
			policies:    []Policy{policy("github", AvailabilityDisabled)},
			grants:      []Grant{grant("github", 7, AvailabilityEnabled)},
			wantAllowed: false,
			wantSource:  SourceDeployment,
		},
		// A grant filed for a DIFFERENT project must not reach this one. This is
		// the "grant to one project only" case seen from the other side.
		"a grant for another project does not apply": {
			policies:    []Policy{policy("github", AvailabilityRestricted)},
			grants:      []Grant{grant("github", 99, AvailabilityEnabled)},
			wantAllowed: false,
			wantSource:  SourceDeployment,
		},
		// Unreachable through the database CHECK. It resolves to ALLOW, because
		// a row this build cannot read must not take a toolkit away.
		"an unreadable availability serves the default": {
			policies:    []Policy{policy("github", Availability("something later"))},
			wantAllowed: true,
			wantSource:  SourceDefault,
		},
	} {
		testCase := testCase
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			filter := BuildFilter(7, testCase.policies, testCase.grants)
			require.Equal(t, testCase.wantAllowed, filter.Allows("github"))
			decision := filter.Decision("github")
			require.Equal(t, testCase.wantAllowed, decision.Allowed)
			require.Equal(t, testCase.wantSource, decision.Source)
			// A type nobody decided about is never affected by another type's row.
			require.True(t, filter.Allows("jira"))
		})
	}
}

// The filter subtracts from a catalogue it does not author, so a type outside
// its rows is served. Pinned because the opposite reading would make every new
// SDK type invisible until an operator enabled it by hand.
func TestUnknownTypeIsAllowed(t *testing.T) {
	t.Parallel()

	filter := BuildFilter(7, []Policy{policy("github", AvailabilityDisabled)}, nil)
	require.False(t, filter.Allows("github"))
	require.True(t, filter.Allows("salesforce"))
	require.Equal(t, 1, filter.Len())
}

func TestAvailabilityParsing(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"enabled", " disabled ", "restricted"} {
		parsed, err := ParseAvailability(value)
		require.NoError(t, err)
		require.NotEmpty(t, parsed)
	}
	_, err := ParseAvailability("default")
	require.Error(t, err, "`default` is the absence of a row, never a stored value")
	_, err = ParseAvailability("")
	require.Error(t, err)

	// `restricted` is meaningless on a grant: a grant IS the exception.
	_, err = ParseGrantAvailability("restricted")
	require.Error(t, err)
	_, err = ParseGrantAvailability("")
	require.Error(t, err)
	denied, err := ParseGrantAvailability("disabled")
	require.NoError(t, err)
	require.Equal(t, AvailabilityDisabled, denied)
	parsed, err := ParseGrantAvailability("enabled")
	require.NoError(t, err)
	require.Equal(t, AvailabilityEnabled, parsed)
}

func TestValidation(t *testing.T) {
	t.Parallel()

	_, err := ValidateReason("   ")
	require.Error(t, err, "whitespace is not a reason")
	var validation PolicyValidationError
	require.ErrorAs(t, err, &validation)
	require.NotEmpty(t, validation.Error())

	trimmed, err := ValidateReason("  a reason  ")
	require.NoError(t, err)
	require.Equal(t, "a reason", trimmed)

	_, err = ValidateReason(string(make([]byte, maxReasonLength+1)))
	require.Error(t, err)

	// NO NORMALISATION. The key must match the served catalogue byte for byte,
	// so `Kubernetes` must not become `kubernetes` here.
	key, err := ValidateToolkitType(" Kubernetes ")
	require.NoError(t, err)
	require.Equal(t, "Kubernetes", key)

	_, err = ValidateToolkitType("  ")
	require.Error(t, err)
	_, err = ValidateToolkitType(string(make([]byte, maxToolkitTypeLength+1)))
	require.Error(t, err)
}
