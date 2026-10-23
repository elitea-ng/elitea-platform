package configurations

// THE COMPATIBILITY LIST DROPPED SIX OF ITS NINE QUERY PARAMETERS.
//
// `Handler.List` read `limit`, `offset` and `section`. The shipped clients also
// send `type`, `query`, `sort_by`, `sort_order`, `include_shared`,
// `shared_offset` and `shared_limit`, and every dropped one changed the answer:
//
//   - The SharePoint credential control asks for `type=sharepoint`
//     (features/toolkits/sharepoint/lib/hooks/useResolvedSharepointConfig.hooks.ts).
//     It received the newest 20 rows of EVERY type, so in a project with more
//     than one page of configurations the credential it needs is absent and the
//     control renders as if it did not exist.
//   - The `shared` block read `shared = true` in the CALLER's schema, so it
//     never carried a public credential, and the main page read
//     `shared = false`, so a credential left the AI-Configuration screen the
//     moment the user shared it.
//   - `shared_offset` and `shared_limit` were hardcoded to 0 and 20.
//
// These cases pin the parsing, the WHERE clause and the ORDER BY, which is
// where the parameters were lost. They need no database.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

func TestListQueryCarriesEveryParameterTheClientSends(t *testing.T) {
	values, err := url.ParseQuery(
		"section=llm&type=open_ai&type=azure_openai&query=prod&limit=5&offset=10" +
			"&include_shared=TRUE&shared_offset=40&shared_limit=7&sort_by=elitea_title&sort_order=asc")
	if err != nil {
		t.Fatalf("parse the query: %v", err)
	}
	request := parseConfigurationListQuery(values)

	if len(request.types) != 2 || request.types[0] != "open_ai" {
		t.Fatalf("types = %v, want the two the client sent", request.types)
	}
	if request.search != "prod" {
		t.Fatalf("search = %q, want %q", request.search, "prod")
	}
	if request.limit != 5 || request.offset != 10 {
		t.Fatalf("limit/offset = %d/%d, want 5/10", request.limit, request.offset)
	}
	// EqualFold, as the reviewed route reads it (read.go).
	if !request.includeShared {
		t.Fatal("include_shared=TRUE must be read case-insensitively")
	}
	if request.sharedOffset != 40 || request.sharedLimit != 7 {
		t.Fatalf("shared page = %d/%d, want 40/7. Both were hardcoded to 0 and 20.",
			request.sharedOffset, request.sharedLimit)
	}
	if request.sortBy != "elitea_title" || request.sortOrder != "asc" {
		t.Fatalf("sort = %s %s, want elitea_title asc", request.sortBy, request.sortOrder)
	}
}

func TestListQueryAppliesTheDefaultsAndTheClamp(t *testing.T) {
	request := parseConfigurationListQuery(url.Values{})
	if request.limit != defaultConfigurationListLimit || request.offset != 0 {
		t.Fatalf("empty query = limit %d offset %d", request.limit, request.offset)
	}
	if request.includeShared {
		t.Fatal("include_shared defaults to false")
	}

	clamped := parseConfigurationListQuery(url.Values{
		"limit":        []string{"100000"},
		"offset":       []string{"-3"},
		"shared_limit": []string{"100000"},
	})
	if clamped.limit != maxConfigurationListLimit || clamped.sharedLimit != maxConfigurationListLimit {
		t.Fatalf("limits = %d/%d, want both clamped to %d",
			clamped.limit, clamped.sharedLimit, maxConfigurationListLimit)
	}
	if clamped.offset != 0 {
		t.Fatalf("offset = %d, want 0 for a negative value", clamped.offset)
	}
}

func TestListFilterBindsTypeAndSearch(t *testing.T) {
	request := parseConfigurationListQuery(url.Values{
		"section": []string{"ai_credentials"},
		"type":    []string{"sharepoint"},
		"query":   []string{"prod"},
	})
	clause, args := configurationRowFilter(request, 1)

	if !strings.Contains(clause, "type = ANY($2)") {
		t.Fatalf("clause = %q, and it carries no type predicate. A picker that asks for one "+
			"type receives every type.", clause)
	}
	if !strings.Contains(clause, "label ILIKE ('%' || $3 || '%')") {
		t.Fatalf("clause = %q, and it carries no label search", clause)
	}
	if len(args) != 3 {
		t.Fatalf("args = %v, want one per placeholder", args)
	}
	// The search term is BOUND, never interpolated.
	if strings.Contains(clause, "prod") {
		t.Fatalf("clause = %q, and it holds the caller's search text", clause)
	}
}

func TestListFilterIsEmptyWithoutParameters(t *testing.T) {
	clause, args := configurationRowFilter(parseConfigurationListQuery(url.Values{}), 1)
	if clause != retiredConfigurationTypeFilter || len(args) != 0 {
		t.Fatalf("clause = %q args = %v, want only the retired-type exclusion", clause, args)
	}
}

// A row of the retired `pgvector` type is kept in the table but never listed,
// whatever the caller filters on: the exclusion is a constant, not an argument.
func TestListFilterAlwaysExcludesTheRetiredPgvectorType(t *testing.T) {
	for _, values := range []url.Values{
		{},
		{"section": {"vectorstorage"}},
		{"type": {"pgvector"}},
	} {
		clause, _ := configurationRowFilter(parseConfigurationListQuery(values), 1)
		if !strings.Contains(clause, retiredConfigurationTypeFilter) {
			t.Fatalf("query %v: clause = %q, want the retired type excluded", values, clause)
		}
	}
}

// The sort column is interpolated into the ORDER BY, so an unrecognised value
// must never reach SQL. It falls back to created_at rather than answering 400,
// because the reference ignores an unknown sort key.
func TestSortByIsWhitelistedAndNeverReachesSQL(t *testing.T) {
	injected := "created_at; DROP TABLE configuration"
	if got := configurationListSortBy(injected); got != "created_at" {
		t.Fatalf("sort_by %q resolved to %q", injected, got)
	}
	clause := configurationOrderBy(configurationListSortBy(injected), configurationListSortOrder(""))
	if strings.Contains(clause, "DROP") {
		t.Fatalf("ORDER BY = %q", clause)
	}
	if clause != "ORDER BY created_at DESC, id ASC" {
		t.Fatalf("default ORDER BY = %q, want the stable created_at page", clause)
	}
	for _, column := range []string{"id", "elitea_title", "type", "section", "updated_at"} {
		if got := configurationListSortBy(column); got != column {
			t.Fatalf("sort_by %q resolved to %q; the whitelist is too narrow", column, got)
		}
	}
	// A nullable column keeps the NULLS LAST the reviewed query gives it.
	if got := configurationOrderBy("label", "asc"); got != "ORDER BY label ASC NULLS LAST, id ASC" {
		t.Fatalf("ORDER BY label asc = %q", got)
	}
}

// The shared block reads the PUBLIC project's schema, or no schema at all.
func TestSharedBlockReadsThePublicProjectOnly(t *testing.T) {
	shared := parseConfigurationListQuery(url.Values{"include_shared": []string{"true"}})

	unwired := &Handler{}
	if _, ok := unwired.sharedConfigurationSchema("7", shared); ok {
		t.Fatal("with no public project id the block must stay empty, not read the caller's schema")
	}

	wired := &Handler{publicProjectID: 1}
	schema, ok := wired.sharedConfigurationSchema("7", shared)
	// The block reads the PUBLIC project's schema, as a quoted identifier
	// ready to interpolate with %s. The expected value is built the same way
	// the handlers build theirs, so the test cannot drift from the quoting.
	wantSchema, err := tenantschema.QuoteInt(1)
	if err != nil {
		t.Fatalf("QuoteInt(1) failed: %v", err)
	}
	if !ok || schema != wantSchema {
		t.Fatalf("schema = %q ok = %v, want %q, the public project's schema", schema, ok, wantSchema)
	}
	// The public project does not read its own rows as "shared with me".
	if _, ok := wired.sharedConfigurationSchema("1", shared); ok {
		t.Fatal("the public project must not receive a shared block of its own rows")
	}
	// No include_shared, no block.
	if _, ok := wired.sharedConfigurationSchema("7", parseConfigurationListQuery(url.Values{})); ok {
		t.Fatal("the block must be served only when include_shared is true")
	}
}

// A missing schema is the ONLY error a list may answer with an empty page.
func TestOnlyAMissingSchemaCountsAsAnEmptyProject(t *testing.T) {
	for _, code := range []string{"3F000", "42P01"} {
		if !configurationSchemaMissing(&pgconn.PgError{Code: code}) {
			t.Fatalf("SQLSTATE %s must read as a schema that does not exist yet", code)
		}
	}
	// 53300 too_many_connections: a real outage, and the handler answered it
	// with `{"items":[],"total":0}` and HTTP 200.
	if configurationSchemaMissing(&pgconn.PgError{Code: "53300"}) {
		t.Fatal("a pool failure must not read as an empty project")
	}
	if configurationSchemaMissing(errors.New("connection reset by peer")) {
		t.Fatal("a transport failure must not read as an empty project")
	}
}

// A duplicate elitea_title is a client mistake, not a server fault.
func TestADuplicateTitleIsABadRequestWithItsField(t *testing.T) {
	duplicate := &pgconn.PgError{Code: "23505", ConstraintName: "configuration_elitea_title_key"}

	created := httptest.NewRecorder()
	writeConfigurationCreateFailure(t.Context(), created, "7", duplicate)
	if created.Code != http.StatusBadRequest {
		t.Fatalf("create status = %d, want %d. A duplicate name reported a server fault.",
			created.Code, http.StatusBadRequest)
	}
	if !strings.Contains(created.Body.String(), "Configuration already exists") ||
		!strings.Contains(created.Body.String(), "elitea_title") {
		t.Fatalf("create body = %s, want the reviewed twin's body", created.Body.String())
	}

	updated := httptest.NewRecorder()
	writeConfigurationUpdateFailure(t.Context(), updated, "7", "11", duplicate)
	if updated.Code != http.StatusBadRequest {
		t.Fatalf("update status = %d, want %d. A rename onto a taken name reported a missing row.",
			updated.Code, http.StatusBadRequest)
	}
}

// Only the elitea_title constraint is a duplicate title.
//
// The match is positive. A negative match on the uuid constraint labels every
// other 23505 a duplicate title, and `configuration_pkey` is the one that
// reaches a user: a SERIAL sequence behind the maximum id raises it.
func TestOnlyTheTitleConstraintIsReportedAsADuplicateTitle(t *testing.T) {
	for _, constraint := range []string{"configuration_uuid_key", "configuration_pkey"} {
		if configurationTitleConflict(&pgconn.PgError{Code: "23505", ConstraintName: constraint}) {
			t.Fatalf("%s must not be labelled a duplicate title", constraint)
		}
	}
	if !configurationTitleConflict(&pgconn.PgError{Code: "23505", ConstraintName: "configuration_elitea_title_key"}) {
		t.Fatal("the elitea_title constraint must be recognised")
	}
}

// A primary-key collision is a server fault, so the route answers 500.
func TestAPrimaryKeyCollisionAnswersAServerFault(t *testing.T) {
	collision := &pgconn.PgError{Code: "23505", ConstraintName: "configuration_pkey"}

	created := httptest.NewRecorder()
	writeConfigurationCreateFailure(t.Context(), created, "7", collision)
	if created.Code != http.StatusInternalServerError {
		t.Fatalf("create status = %d, want %d. A stale sequence reported a client mistake.",
			created.Code, http.StatusInternalServerError)
	}
	if strings.Contains(created.Body.String(), "elitea_title") {
		t.Fatalf("create body = %s, want no title field for a server fault", created.Body.String())
	}

	updated := httptest.NewRecorder()
	writeConfigurationUpdateFailure(t.Context(), updated, "7", "11", collision)
	if updated.Code != http.StatusInternalServerError {
		t.Fatalf("update status = %d, want %d", updated.Code, http.StatusInternalServerError)
	}
}

// Only an absent row is a 404 on update. A database failure is a 500.
func TestUpdateSeparatesAnAbsentRowFromAFailure(t *testing.T) {
	absent := httptest.NewRecorder()
	writeConfigurationUpdateFailure(t.Context(), absent, "7", "11", pgx.ErrNoRows)
	if absent.Code != http.StatusNotFound {
		t.Fatalf("absent row status = %d, want %d", absent.Code, http.StatusNotFound)
	}

	broken := httptest.NewRecorder()
	writeConfigurationUpdateFailure(t.Context(), broken, "7", "11", errors.New("connection reset by peer"))
	if broken.Code != http.StatusInternalServerError {
		t.Fatalf("failure status = %d, want %d. A failed update reported a missing row.",
			broken.Code, http.StatusInternalServerError)
	}
}

// An oversized filter is refused, and a normal filter passes.
//
// The type list becomes `type = ANY($n)` and the search string becomes a
// leading-wildcard ILIKE. Without a bound, one request holds thousands of
// values and the tenant table scans for each of them.
func TestAnOversizedListFilterIsRefused(t *testing.T) {
	tooMany := make([]string, maxConfigurationFilterValues+1)
	for i := range tooMany {
		tooMany[i] = "llm"
	}
	longValue := strings.Repeat("x", maxConfigurationFilterLength+1)
	longQuery := strings.Repeat("x", maxConfigurationQueryLength+1)

	refused := map[string]url.Values{
		"too many types":    {"type": tooMany},
		"too many sections": {"section": tooMany},
		"long type value":   {"type": []string{longValue}},
		"long section":      {"section": []string{longValue}},
		"long query":        {"query": []string{longQuery}},
	}
	for name, values := range refused {
		t.Run(name, func(t *testing.T) {
			if configurationListQueryInBounds(parseConfigurationListQuery(values)) {
				t.Fatal("the filter is outside the bounds of the reviewed service, so the route must refuse it")
			}
		})
	}

	accepted := url.Values{
		"type":    []string{"llm", "embedding"},
		"section": []string{"llm"},
		"query":   []string{strings.Repeat("x", maxConfigurationQueryLength)},
	}
	if !configurationListQueryInBounds(parseConfigurationListQuery(accepted)) {
		t.Fatal("a filter inside the bounds must pass")
	}
	if !configurationListQueryInBounds(parseConfigurationListQuery(url.Values{})) {
		t.Fatal("an empty filter must pass")
	}
}
