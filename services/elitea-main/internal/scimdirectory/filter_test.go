package scimdirectory

// What these tests hold in place.
//
// The dangerous half of a filter implementation is not the parsing, it is what
// happens to an expression the server does not understand. Returning the whole
// directory is the obvious implementation and the worst one: the client asked
// "is there an account with this address", got a list with somebody else at the
// top, and updates the wrong person's row.
//
// Every refusal below is one expression that would have been silently ignored.

import (
	"errors"
	"strings"
	"testing"
)

func TestTheSupportedComparisonsParse(t *testing.T) {
	for _, expression := range []string{
		`userName eq "alice@corp.com"`,
		`userName EQ "alice@corp.com"`,
		`UserName eq "alice@corp.com"`,
		`externalId eq "00u1abc"`,
		`displayName sw "Ali"`,
		`active eq true`,
		`active ne false`,
		`id eq "42"`,
	} {
		if _, err := ParseFilter(expression); err != nil {
			t.Fatalf("ParseFilter(%q): %v", expression, err)
		}
	}
}

// An empty filter is the whole directory, which is how a client pages it. A
// zero Filter that matched nothing would answer that request with an empty page
// and the client would conclude the deployment has no users.
func TestNoFilterMatchesEverything(t *testing.T) {
	filter, err := ParseFilter("")
	if err != nil {
		t.Fatalf("ParseFilter(\"\"): %v", err)
	}
	clause, arguments := filter.clause()
	if clause != "" || len(arguments) != 0 {
		t.Fatalf("the empty filter produced %q with %d arguments", clause, len(arguments))
	}
}

/* ── the refusals ──────────────────────────────────────────────────────── */

func requireUnsupported(t *testing.T, expression, mustMention string) {
	t.Helper()
	_, err := ParseFilter(expression)
	var unsupported UnsupportedFilterError
	if !errors.As(err, &unsupported) {
		t.Fatalf("ParseFilter(%q) returned %v, want an UnsupportedFilterError", expression, err)
	}
	if !strings.Contains(unsupported.Reason, mustMention) {
		t.Fatalf("the reason %q does not mention %q", unsupported.Reason, mustMention)
	}
}

// `or` and `not` are REFUSED, not half-applied: answering only one side of
// `userName eq "a" or userName eq "b"` would hide the other account.
func TestOrAndNotAreRefused(t *testing.T) {
	requireUnsupported(t, `userName eq "a" or userName eq "b"`, "and/or/not")
	requireUnsupported(t, `not(active eq true)`, "and/or/not")
	requireUnsupported(t, `(userName eq "a")`, "and/or/not")
	requireUnsupported(t, `userName eq "a" and (active eq true)`, "and/or/not")
}

// Operators and brackets INSIDE a quoted value are part of the value.
func TestOperatorsInsideQuotedValuesAreIgnored(t *testing.T) {
	for _, testCase := range []struct{ expression, value string }{
		{`displayName eq "Ops (EU) and Sales"`, "ops (eu) and sales"},
		{`displayName eq "a [b] or not(c)"`, "a [b] or not(c)"},
		{`userName eq "o'brien and (sons)@corp.com"`, "o'brien and (sons)@corp.com"},
		{`displayName eq "say \"and\" now"`, `say "and" now`},
	} {
		filter, err := ParseFilter(testCase.expression)
		if err != nil {
			t.Fatalf("ParseFilter(%q): %v", testCase.expression, err)
		}
		_, arguments := filter.clause()
		if len(arguments) != 1 || arguments[0] != testCase.value {
			t.Fatalf("ParseFilter(%q) bound %v, want %q", testCase.expression, arguments, testCase.value)
		}
	}
}

func TestAndOfTwoComparisonsRendersBothAsBoundParameters(t *testing.T) {
	filter, err := ParseFilter(`userName eq "Alice@corp.com" AND active eq true`)
	if err != nil {
		t.Fatalf("ParseFilter: %v", err)
	}
	clause, arguments := filter.clause()
	if clause != " WHERE lower(lower(account.email)) = $1 AND (NOT account.suspended) = $2" {
		t.Fatalf("unexpected clause %q", clause)
	}
	if len(arguments) != 2 || arguments[0] != "alice@corp.com" || arguments[1] != true {
		t.Fatalf("unexpected arguments %v", arguments)
	}
}

func TestAnUnclosedQuoteIsRefused(t *testing.T) {
	requireUnsupported(t, `userName eq "abc`, "not closed")
}

// An attribute this directory does not store cannot be filtered on. Answering
// the whole directory would say "yes, somebody has that title".
func TestAnUnknownAttributeIsRefused(t *testing.T) {
	requireUnsupported(t, `title eq "Engineer"`, "userName, externalId")
}

// `pr` (present), `gt`, `le` and the rest are not implemented, and an
// unimplemented operator must not degrade to equality.
func TestAnUnimplementedOperatorIsRefused(t *testing.T) {
	requireUnsupported(t, `userName pr`, "attribute operator value")
	requireUnsupported(t, `displayName gt "M"`, "eq, ne, co, sw and ew")
}

func TestActiveOnlyComparesWithABoolean(t *testing.T) {
	requireUnsupported(t, `active eq "maybe"`, "true or false")
	requireUnsupported(t, `active co "tru"`, "eq or ne")
}

/* ── the clause it renders ─────────────────────────────────────────────── */

// The value is always a bound parameter, never concatenated. It arrives from a
// query string on an externally-controlled request.
func TestTheValueIsAlwaysABoundParameter(t *testing.T) {
	filter, err := ParseFilter(`userName eq "alice@corp.com'; DROP TABLE auth_core__user --"`)
	if err != nil {
		t.Fatalf("ParseFilter: %v", err)
	}
	clause, arguments := filter.clause()
	if strings.Contains(clause, "DROP") {
		t.Fatalf("the clause carries the caller's value: %q", clause)
	}
	if len(arguments) != 1 {
		t.Fatalf("the clause bound %d arguments, want 1", len(arguments))
	}
}

// Comparison is case-insensitive on both sides. An identity provider that
// created `Alice@corp.com` and later filters on `alice@corp.com` must find it;
// a case-sensitive match would report no such user and the client would create
// a second account for the same person.
func TestComparisonIsCaseInsensitive(t *testing.T) {
	filter, err := ParseFilter(`userName eq "Alice@Corp.com"`)
	if err != nil {
		t.Fatalf("ParseFilter: %v", err)
	}
	_, arguments := filter.clause()
	if arguments[0] != "alice@corp.com" {
		t.Fatalf("the bound value is %v, want it folded to lower case", arguments[0])
	}
}

// Without escaping, a `co` filter of `%` matches every account — which turns
// "does an account containing this string exist" into "yes" for any string.
func TestSubstringWildcardsAreEscaped(t *testing.T) {
	filter, err := ParseFilter(`displayName co "100%_"`)
	if err != nil {
		t.Fatalf("ParseFilter: %v", err)
	}
	_, arguments := filter.clause()
	if arguments[0] != `%100\%\_%` {
		t.Fatalf("the bound pattern is %v, want the caller's wildcards escaped", arguments[0])
	}
}

// The address is folded on the way in as well as on the way out, so a push of
// `Alice@Corp.com` and a later filter for `alice@corp.com` name one account.
func TestUserNameNormalisationMatchesTheFilter(t *testing.T) {
	if NormalizeUserName("  Alice@Corp.com ") != "alice@corp.com" {
		t.Fatalf("NormalizeUserName did not fold the address")
	}
}
