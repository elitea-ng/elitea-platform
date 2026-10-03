package scimdirectory

// The SCIM filter subset this directory answers.
//
// # Why a subset, and why an unsupported filter is REFUSED
//
// RFC 7644 §3.4.2.2 defines a full expression language: grouping, `and`/`or`
// /`not`, complex attribute paths, and nine operators. Almost none of it is
// used in practice — an identity provider provisioning users sends
// `userName eq "…"` to find out whether it has already created someone, and
// `externalId eq "…"` to find what it created.
//
// The dangerous half is not the parsing, it is what a server does with a filter
// it does not understand. Returning the WHOLE directory is the obvious
// implementation and the worst one: the client asked "is there an account with
// this address", got a list with somebody else at the top, and updates the wrong
// person's row. So an expression this file cannot represent is refused with 400
// and the reason, never approximated and never ignored.
//
// # What is supported
//
//	userName    eq | ne | co | sw | ew  "value"    (users)
//	externalId  eq | ne | co | sw | ew  "value"    (users and groups)
//	displayName eq | ne | co | sw | ew  "value"    (users and groups)
//	id          eq                      "value"    (users and groups)
//	active      eq | ne                 true | false (users)
//
// Attribute names are matched case-insensitively, as RFC 7644 requires. Comparisons may be
// joined with `and`. Nothing else is: no `or`, no `not`, no `pr`, no grouping. Each of those is a
// separate, testable addition, and each would be a lie if it were accepted and
// half-applied.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// comparison is one `attribute operator value` term.
type comparison struct {
	attribute string
	operator  string
	value     string
}

// Filter is one parsed comparison, a conjunction of them, or the empty filter
// that matches everything.
type Filter struct {
	terms []comparison
	// columns is the attribute-to-column map the expression was parsed
	// against, carried so `clause` renders the SQL of the resource the filter
	// was read FOR. A users filter and a groups filter are the same shape over
	// different tables, and a filter that rendered the other resource's columns
	// would be a query that fails at the database — or, worse, one that
	// succeeds against a column of the same name and answers about the wrong
	// rows.
	columns map[string]string
	// present is false for the empty filter. A zero Filter must match
	// everything, because "no filter" is how a client asks for the whole
	// directory — and a zero value that matched nothing would answer that
	// request with an empty page.
	present bool
}

// UnsupportedFilterError names the expression this directory cannot represent.
//
// It is a distinct type so the HTTP layer answers 400 with `scimType:
// invalidFilter`, which is the code RFC 7644 defines for exactly this, and
// keeps every other failure a 500.
type UnsupportedFilterError struct {
	Expression string
	Reason     string
}

func (e UnsupportedFilterError) Error() string {
	return fmt.Sprintf("unsupported filter %q: %s", e.Expression, e.Reason)
}

// userAttributes maps the SCIM User attribute name to its column, and is the
// closed set the /Users parser accepts.
var userAttributes = map[string]string{
	"username":    "lower(account.email)",
	"externalid":  "COALESCE(scim.external_id, '')",
	"displayname": "COALESCE(account.name, '')",
	"id":          "account.id::text",
	"active":      "NOT account.suspended",
}

// groupAttributes is the same closed set for the /Groups resource.
//
// `members` is deliberately absent. An identity provider that filtered groups by
// member would be asking a question this parser cannot answer, and the answer it
// would get from an ignored filter — every group — is the one that silently
// grants access. It is refused by name, like every other unsupported attribute.
var groupAttributes = map[string]string{
	"displayname": "binding.display_name",
	"externalid":  "binding.external_id",
	"id":          "binding.id::text",
}

// ParseFilter reads one `filter` query parameter of a /Users listing.
//
// An empty string is the empty filter, not an error: a listing with no filter is
// the ordinary way to page the whole directory.
func ParseFilter(expression string) (Filter, error) {
	return parseFilter(expression, userAttributes)
}

// ParseGroupFilter reads one `filter` query parameter of a /Groups listing.
func ParseGroupFilter(expression string) (Filter, error) {
	return parseFilter(expression, groupAttributes)
}

// maxFilterLength and maxFilterTerms bound one expression. A provisioning client
// sends one or two comparisons; the bounds exist so an authenticated machine
// credential cannot ask for an arbitrarily long conjunction, which becomes one
// SQL predicate (and one bind parameter) per term.
const (
	maxFilterLength = 2048
	maxFilterTerms  = 10
)

const compoundReason = "this directory answers comparisons joined by `and`; " +
	"grouping and the or/not operators (and/or/not beyond a plain `and`) are not implemented"

func parseFilter(expression string, columns map[string]string) (Filter, error) {
	trimmed := strings.TrimSpace(expression)
	if trimmed == "" {
		return Filter{}, nil
	}

	unsupported := func(reason string) (Filter, error) {
		return Filter{}, UnsupportedFilterError{Expression: expression, Reason: reason}
	}
	if len(trimmed) > maxFilterLength {
		// The expression is NOT echoed back: it is the oversized thing.
		return Filter{}, UnsupportedFilterError{
			Expression: trimmed[:64] + "…",
			Reason:     "the filter is longer than " + strconv.Itoa(maxFilterLength) + " characters",
		}
	}

	tokens, err := tokenizeFilter(trimmed)
	if err != nil {
		return unsupported(err.Error())
	}

	// Split on `and` TOKENS. An operator inside a quoted value is part of the
	// value and never reaches here as a bare word, which is what the previous
	// substring scan got wrong for `displayName eq "Ops (EU) and Sales"`.
	var groups [][]filterToken
	current := []filterToken{}
	for _, token := range tokens {
		if token.kind == tokenParen {
			return unsupported(compoundReason)
		}
		if token.kind == tokenWord {
			switch strings.ToLower(token.text) {
			case "or", "not":
				return unsupported(compoundReason)
			case "and":
				groups = append(groups, current)
				if len(groups) >= maxFilterTerms {
					return unsupported("this directory answers at most " +
						strconv.Itoa(maxFilterTerms) + " comparisons joined by `and`")
				}
				current = []filterToken{}
				continue
			}
		}
		current = append(current, token)
	}
	groups = append(groups, current)

	terms := make([]comparison, 0, len(groups))
	for _, group := range groups {
		term, reason := parseComparison(group, columns)
		if reason != "" {
			return unsupported(reason)
		}
		terms = append(terms, term)
	}
	return Filter{terms: terms, present: true, columns: columns}, nil
}

func parseComparison(tokens []filterToken, columns map[string]string) (comparison, string) {
	if len(tokens) != 3 || tokens[0].kind != tokenWord || tokens[1].kind != tokenWord {
		return comparison{}, "expected an expression of the form `attribute operator value`"
	}
	attribute := strings.ToLower(tokens[0].text)
	if _, ok := columns[attribute]; !ok {
		// The message names what THIS resource can be filtered on, read from
		// the same map the parser accepts against, so it can never advertise an
		// attribute the parser then refuses.
		return comparison{}, "this directory can only filter on " + attributeList(columns)
	}
	operator := strings.ToLower(tokens[1].text)
	value := tokens[2].text

	if attribute == "active" {
		if operator != "eq" && operator != "ne" {
			return comparison{}, "active can only be compared with eq or ne"
		}
		if value != "true" && value != "false" {
			return comparison{}, "active can only be compared with true or false"
		}
		return comparison{attribute: attribute, operator: operator, value: value}, ""
	}
	switch operator {
	case "eq", "ne", "co", "sw", "ew":
	default:
		return comparison{}, "this directory implements the eq, ne, co, sw and ew operators"
	}
	if attribute == "id" && operator != "eq" {
		return comparison{}, "id can only be compared with eq"
	}
	return comparison{attribute: attribute, operator: operator, value: value}, ""
}

type filterTokenKind int

const (
	tokenWord filterTokenKind = iota
	tokenQuoted
	tokenParen
)

type filterToken struct {
	kind filterTokenKind
	text string
}

// tokenizeFilter splits an expression into words, quoted strings and
// parentheses. A quoted string is one token whatever it contains, with the JSON
// escapes RFC 7644 §3.4.2.2 uses.
func tokenizeFilter(expression string) ([]filterToken, error) {
	var tokens []filterToken
	runes := []rune(expression)
	for i := 0; i < len(runes); {
		switch r := runes[i]; r {
		case ' ', '\t', '\n', '\r':
			i++
		case '(', ')', '[', ']':
			tokens = append(tokens, filterToken{kind: tokenParen, text: string(r)})
			i++
		case '"':
			j := i + 1
			for j < len(runes) && runes[j] != '"' {
				if runes[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(runes) {
				return nil, errors.New("a quoted value is not closed")
			}
			raw := string(runes[i : j+1])
			var value string
			if err := json.Unmarshal([]byte(raw), &value); err != nil {
				value = string(runes[i+1 : j])
			}
			tokens = append(tokens, filterToken{kind: tokenQuoted, text: value})
			i = j + 1
		default:
			j := i
			for j < len(runes) && !strings.ContainsRune(" \t\n\r()[]\"", runes[j]) {
				j++
			}
			tokens = append(tokens, filterToken{kind: tokenWord, text: string(runes[i:j])})
			i = j
		}
	}
	return tokens, nil
}

// attributeList renders a closed set as the SCIM attribute names an operator
// configured, in a stable order.
func attributeList(columns map[string]string) string {
	names := make([]string, 0, len(columns))
	for _, name := range []string{"username", "externalid", "displayname", "id", "active"} {
		if _, ok := columns[name]; ok {
			names = append(names, scimAttributeNames[name])
		}
	}
	return strings.Join(names, ", ")
}

// scimAttributeNames is the spelling a client sends and an operator configures.
// The parser folds case to match; a refusal that echoed the folded name would
// send an operator looking for `externalid` in a provider that calls it
// `externalId`.
var scimAttributeNames = map[string]string{
	"username":    "userName",
	"externalid":  "externalId",
	"displayname": "displayName",
	"id":          "id",
	"active":      "active",
}

// clause renders the filter as SQL and its arguments.
//
// The value is ALWAYS a bound parameter. It arrives from a query string on an
// authenticated but externally-controlled request, and the column is chosen from
// the closed map above — so neither half of the comparison is ever concatenated
// from caller input.
func (f Filter) clause() (string, []any) {
	if !f.present {
		return "", nil
	}
	conditions := make([]string, 0, len(f.terms))
	arguments := make([]any, 0, len(f.terms))
	for _, term := range f.terms {
		position := "$" + strconv.Itoa(len(arguments)+1)
		condition, argument := term.render(f.columns[term.attribute], position)
		conditions = append(conditions, condition)
		arguments = append(arguments, argument)
	}
	return " WHERE " + strings.Join(conditions, " AND "), arguments
}

func (t comparison) render(column, position string) (string, any) {
	if t.attribute == "active" {
		wanted := t.value == "true"
		if t.operator == "ne" {
			wanted = !wanted
		}
		return "(" + column + ") = " + position, wanted
	}

	// Comparison is case-insensitive on both sides. SCIM defines `userName` as
	// case-insensitive, and an identity provider that created `Alice@corp.com`
	// and later filters on `alice@corp.com` must find it — a case-sensitive
	// match would report no such user and the client would create a second
	// account.
	pattern := strings.ToLower(t.value)
	switch t.operator {
	case "eq":
		return "lower(" + column + ") = " + position, pattern
	case "ne":
		return "lower(" + column + ") <> " + position, pattern
	case "co":
		return "lower(" + column + ") LIKE " + position, "%" + escapeLike(pattern) + "%"
	case "sw":
		return "lower(" + column + ") LIKE " + position, escapeLike(pattern) + "%"
	default: // "ew"
		return "lower(" + column + ") LIKE " + position, "%" + escapeLike(pattern)
	}
}

// escapeLike neutralises the wildcards a caller could otherwise smuggle into a
// substring match.
//
// Without it, a `co` filter of `%` matches every account, which turns "does an
// account containing this string exist" into "yes" for any string. The escape
// character is the backslash, which is PostgreSQL's default for LIKE.
func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}
