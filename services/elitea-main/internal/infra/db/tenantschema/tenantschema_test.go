package tenantschema

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// hostileProjectIDs are project ids whose text leaves a %q-quoted identifier.
// Each one closes the identifier at the quote that %q writes as \" and turns
// the rest of the value into SQL.
var hostileProjectIDs = []struct {
	name string
	id   string
}{
	{"closes the identifier and joins another table", `1".configuration, centry.project x --`},
	{"closes the identifier and ends the statement", `1"; DROP TABLE centry.project; --`},
	{"closes the identifier and reads the vault", `1".configuration UNION SELECT * FROM centry.secrets --`},
	{"bare quote", `1"`},
	{"quote first", `"1`},
	{"doubled quote", `1""`},
	{"backslash and quote", `1\"`},
	{"not a number", "abc"},
	{"empty", ""},
	{"negative", "-1"},
	{"whitespace", " 1"},
	{"NUL byte", "1\x00"},
	{"too long", strings.Repeat("9", MaxProjectIDDigits+1)},
	{"zero is no project", "0"},
	{"leading zero is a second spelling", "007"},
}

// TestQuoteRefusesHostileProjectID is the regression test for issue #543.
//
// It fails before the correction, because the handlers built the schema with
// fmt.Sprintf("p_%s", projectID) and interpolated it with %q, which accepted
// every id below.
func TestQuoteRefusesHostileProjectID(t *testing.T) {
	for _, tc := range hostileProjectIDs {
		t.Run(tc.name, func(t *testing.T) {
			quoted, err := Quote(tc.id)
			if err == nil {
				t.Fatalf("Quote(%q) = %q, want an error: a hostile id reached the statement", tc.id, quoted)
			}
			if quoted != "" {
				t.Errorf("Quote(%q) returned %q with an error; a caller that ignores the error still builds a statement", tc.id, quoted)
			}
			if !errors.Is(err, ErrInvalidProjectID) {
				t.Errorf("Quote(%q) error = %v, want ErrInvalidProjectID", tc.id, err)
			}
			var apiErr *apierr.APIError
			if !errors.As(err, &apiErr) || apiErr.Status != 400 {
				t.Errorf("Quote(%q) must answer 400, not a 500 carrying a SQL error", tc.id)
			}
			if strings.Contains(apiErr.Message, tc.id) && tc.id != "" {
				t.Errorf("Quote(%q) message %q echoes the caller's text back", tc.id, apiErr.Message)
			}
		})
	}
}

// TestQuoteAcceptsRealProjectID proves the correction does not refuse a valid
// project id, and that the identifier it builds is the one the tenant uses.
func TestQuoteAcceptsRealProjectID(t *testing.T) {
	cases := []struct{ id, want string }{
		{"1", `"p_1"`},
		{"7", `"p_7"`},
		{"1234567", `"p_1234567"`},
		{strings.Repeat("9", MaxProjectIDDigits), `"p_` + strings.Repeat("9", MaxProjectIDDigits) + `"`},
	}
	for _, tc := range cases {
		quoted, err := Quote(tc.id)
		if err != nil {
			t.Fatalf("Quote(%q) failed: %v", tc.id, err)
		}
		if quoted != tc.want {
			t.Errorf("Quote(%q) = %q, want %q", tc.id, quoted, tc.want)
		}
		statement := fmt.Sprintf("SELECT data FROM %s.configuration WHERE type = $1", quoted)
		if !strings.Contains(statement, `FROM "p_`+tc.id+`".configuration`) {
			t.Errorf("statement %q does not read the tenant's own schema", statement)
		}
	}
}

// TestGoQuotingDiffersFromSQLQuoting records WHY %q is the wrong tool. It
// measures the two rules against each other rather than reasoning about them.
//
// %q escapes an embedded quote as \" (Go). PostgreSQL wants it doubled, treats
// the backslash as an ordinary character, and therefore ENDS the identifier at
// the quote. Everything after that point is SQL, not a schema name.
func TestGoQuotingDiffersFromSQLQuoting(t *testing.T) {
	const payload = `1".configuration, centry.project x --`
	name := Prefix + payload

	goQuoted := fmt.Sprintf("%q", name)
	if !strings.Contains(goQuoted, `\"`) {
		t.Fatalf("%%q output %q no longer contains \\\"; re-measure the premise of issue #543", goQuoted)
	}

	// The identifier ends at the first quote that is not doubled. For the %q
	// output that is the quote after the backslash, so the identifier is
	// `p_1\` and the caller's text escaped.
	if got := identifierBody(goQuoted); got != `p_1\` {
		t.Errorf("PostgreSQL reads %%q output %q as identifier %q, want `p_1\\`", goQuoted, got)
	}

	sqlQuoted, err := Quote("1")
	if err != nil {
		t.Fatalf("Quote(1) failed: %v", err)
	}
	if got := identifierBody(sqlQuoted); got != "p_1" {
		t.Errorf("Quote(1) = %q reads as identifier %q, want p_1", sqlQuoted, got)
	}
}

// identifierBody returns the identifier PostgreSQL reads from a quoted
// identifier at the start of quoted: it stops at the first quote that is not
// doubled, and it treats a backslash as an ordinary character.
func identifierBody(quoted string) string {
	if len(quoted) == 0 || quoted[0] != '"' {
		return ""
	}
	var body strings.Builder
	for i := 1; i < len(quoted); i++ {
		if quoted[i] != '"' {
			body.WriteByte(quoted[i])
			continue
		}
		if i+1 < len(quoted) && quoted[i+1] == '"' {
			body.WriteByte('"')
			i++
			continue
		}
		break
	}
	return body.String()
}

func TestNameIsUnquoted(t *testing.T) {
	name, err := Name("42")
	if err != nil {
		t.Fatalf("Name(42) failed: %v", err)
	}
	if name != "p_42" {
		t.Errorf("Name(42) = %q, want p_42", name)
	}
	if strings.Contains(name, `"`) {
		t.Errorf("Name(42) = %q must be unquoted; it is bound as a parameter", name)
	}
	for _, tc := range hostileProjectIDs {
		if _, err := Name(tc.id); err == nil {
			t.Errorf("Name(%q) accepted a hostile id", tc.id)
		}
	}
}

func TestQuoteInt(t *testing.T) {
	quoted, err := QuoteInt(3)
	if err != nil {
		t.Fatalf("QuoteInt(3) failed: %v", err)
	}
	if quoted != `"p_3"` {
		t.Errorf("QuoteInt(3) = %q, want \"p_3\"", quoted)
	}
	for _, id := range []int64{0, -1, -9223372036854775808} {
		if _, err := QuoteInt(id); err == nil {
			t.Errorf("QuoteInt(%d) accepted a non-project id", id)
		}
	}
}
