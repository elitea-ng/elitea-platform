package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- fake DB seam for the resolver ---------------------------------------

// fakeModelRow is one row the fake DB returns: an elitea_title alias and the
// raw data JSONB bytes (as stored in the configuration table).
type fakeModelRow struct {
	title string
	data  []byte
	// shared is the row's `shared` column. It defaults to false, which is what
	// an ordinary project-owned row carries.
	shared bool
	// section and type are the row's configuration coordinates. The ZERO VALUE
	// means the chat pair (`llm`/`llm_model`), so a test that does not care
	// about sections reads as it did before the resolver admitted more than one.
	section string
	typ     string
}

// pair returns the row's (section, type), defaulting the zero value to the chat
// pair.
func (r fakeModelRow) pair() (string, string) {
	if r.section == "" && r.typ == "" {
		return "llm", "llm_model"
	}
	return r.section, r.typ
}

// keepAddressableRows drops every row whose (section, type) pair is absent from
// the statement's bind arguments — the fake's stand-in for modelsSQL's join
// against unnest($1, $2). args is (sections []string, types []string); anything
// else leaves the rows untouched, so a malformed call fails on the assertion the
// test actually makes rather than on an empty result.
func keepAddressableRows(rows []fakeModelRow, args []any) []fakeModelRow {
	if len(args) != 2 {
		return rows
	}
	sections, ok := args[0].([]string)
	if !ok {
		return rows
	}
	types, ok := args[1].([]string)
	if !ok || len(types) != len(sections) {
		return rows
	}
	kept := make([]fakeModelRow, 0, len(rows))
	for _, r := range rows {
		rowSection, rowType := r.pair()
		for i := range sections {
			if sections[i] == rowSection && types[i] == rowType {
				kept = append(kept, r)
				break
			}
		}
	}
	return kept
}

// fakeModelDB is a modelRowQuerier test double. It returns rows verbatim and
// can be made to fail (err) or count how many times Query ran (calls) so the
// cache behaviour is observable.
type fakeModelDB struct {
	// rows is the catch-all result, returned for every query unless bySchema
	// carries an entry for the schema the query names.
	rows  []fakeModelRow
	err   error
	calls int
	// bySchema routes rows per project schema ("7", "1", …) so a test can seed
	// different models for the caller's project and for the public project. A
	// schema with no entry yields zero rows, NOT the catch-all.
	bySchema map[string][]fakeModelRow
	// gotSQL records every statement issued, in order, so a test can assert
	// WHICH schemas were read and that the shared predicate was applied.
	gotSQL []string
	// gotArgs records the bind arguments of every statement, in the same order
	// as gotSQL. The section/type pairs the resolver admits travel as bind
	// parameters, not in the statement text, so this is the only place a test
	// can see WHICH configuration sections were read.
	gotArgs [][]any
	// ignoreSharedPredicate makes the fake return unpublished rows even when the
	// statement asks for `shared = true`, simulating a query that lost its
	// predicate. Postgres is modelled faithfully by default.
	ignoreSharedPredicate bool
	// credsBySchema routes ai_credentials rows per project schema (issue #451).
	// A schema with no entry yields no credentials, which is what a model row
	// that links to nothing needs — so every test written before #451 keeps its
	// old behaviour without a change.
	credsBySchema map[string][]fakeCredentialRow
}

// fakeCredentialRow is one ai_credentials row as the model resolver reads it:
// an id, a type, a title and a non-secret api_base. It holds no secret because
// credentialRefsSQL selects none.
type fakeCredentialRow struct {
	id      string
	typ     string
	title   string
	apiBase string
	// shared is the row's `shared` column, used by the public-scope read.
	shared bool
}

func (db *fakeModelDB) Query(_ context.Context, sql string, args ...any) (modelRows, error) {
	db.calls++
	db.gotSQL = append(db.gotSQL, sql)
	db.gotArgs = append(db.gotArgs, args)
	if db.err != nil {
		return nil, db.err
	}
	// The resolver issues two statements with two different column shapes.
	// Match on the section the statement names, exactly as the production
	// statement builds it, so this branch cannot drift away from the code.
	if strings.Contains(sql, credentialSection) {
		creds := db.credsBySchema[modelSchemaProjectOf(sql)]
		if strings.Contains(sql, "shared = true") && !db.ignoreSharedPredicate {
			kept := make([]fakeCredentialRow, 0, len(creds))
			for _, c := range creds {
				if c.shared {
					kept = append(kept, c)
				}
			}
			creds = kept
		}
		return &fakeCredentialRowsIter{rows: creds}, nil
	}
	rows := db.rows
	if db.bySchema != nil {
		rows = db.bySchema[modelSchemaProjectOf(sql)]
	}
	// Model the (section, type) join: a row whose pair the statement did not
	// bind is not visible to the resolver. Without this the fake would return an
	// asr/tts row to a resolver that never asked for one, and a test could not
	// tell the admitted sections apart from the rejected ones.
	rows = keepAddressableRows(rows, args)
	// Model Postgres: `shared = true` really does exclude unpublished rows.
	// Without this an isolation test would prove nothing.
	if strings.Contains(sql, "shared = true") && !db.ignoreSharedPredicate {
		kept := make([]fakeModelRow, 0, len(rows))
		for _, r := range rows {
			if r.shared {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	return &fakeModelRowsIter{rows: rows}, nil
}

// queriesPerScope is the number of statements ONE project scope costs the
// resolver: the credential read of issue #451 and then the model read. A cache
// assertion must count in these units. Counting raw statements instead would
// let a second, unwanted refresh hide inside a number that looked plausible.
const queriesPerScope = 2

// modelStatements returns only the model reads. credentialStatements returns
// only the credential reads. A test that asserts WHICH schema was read in which
// order must filter first: the two statements interleave, so a positional index
// into gotSQL no longer names the scope the test means.
func (db *fakeModelDB) modelStatements() []string {
	return filterStatements(db.gotSQL, false)
}

func (db *fakeModelDB) credentialStatements() []string {
	return filterStatements(db.gotSQL, true)
}

// modelArgs returns the bind arguments of the model reads, in order, so an
// argument assertion reads the statement it means and not the credential read
// that now precedes it.
func (db *fakeModelDB) modelArgs() [][]any {
	out := make([][]any, 0, len(db.gotArgs))
	for i, q := range db.gotSQL {
		if !strings.Contains(q, credentialSection) && i < len(db.gotArgs) {
			out = append(out, db.gotArgs[i])
		}
	}
	return out
}

func filterStatements(all []string, credentials bool) []string {
	out := make([]string, 0, len(all))
	for _, q := range all {
		if strings.Contains(q, credentialSection) == credentials {
			out = append(out, q)
		}
	}
	return out
}

// modelSchemaProjectOf extracts the project id from the `FROM "p_<id>"` clause
// of a statement, so the fake can serve per-project rows.
func modelSchemaProjectOf(sql string) string {
	const marker = `"p_`
	i := strings.Index(sql, marker)
	if i < 0 {
		return ""
	}
	rest := sql[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// fakeModelRowsIter iterates fakeModelRow values as modelRows.
type fakeModelRowsIter struct {
	rows []fakeModelRow
	i    int
}

func (it *fakeModelRowsIter) Next() bool {
	if it.i >= len(it.rows) {
		return false
	}
	it.i++
	return true
}

func (it *fakeModelRowsIter) Scan(dest ...any) error {
	row := it.rows[it.i-1]
	// queryScope() scans (title string, data []byte, shared bool) in that order.
	if len(dest) != 3 {
		return errors.New("unexpected scan arity")
	}
	*dest[0].(*string) = row.title
	*dest[1].(*[]byte) = row.data
	*dest[2].(*bool) = row.shared
	return nil
}

func (it *fakeModelRowsIter) Err() error { return nil }
func (it *fakeModelRowsIter) Close()     {}

// fakeCredentialRowsIter iterates fakeCredentialRow values as modelRows.
type fakeCredentialRowsIter struct {
	rows []fakeCredentialRow
	i    int
}

func (it *fakeCredentialRowsIter) Next() bool {
	if it.i >= len(it.rows) {
		return false
	}
	it.i++
	return true
}

// Scan fills (id string, type string, title string, api_base string) — the column order
// credentialRefs() expects.
func (it *fakeCredentialRowsIter) Scan(dest ...any) error {
	if len(dest) != 4 {
		return errors.New("unexpected credential scan arity")
	}
	row := it.rows[it.i-1]
	*dest[0].(*string) = row.id
	*dest[1].(*string) = row.typ
	*dest[2].(*string) = row.title
	*dest[3].(*string) = row.apiBase
	return nil
}

func (it *fakeCredentialRowsIter) Err() error { return nil }
func (it *fakeCredentialRowsIter) Close()     {}

// rowsErrIter yields no rows but reports a deferred Err() — exercises the
// rows.Err() failure branch that Query cannot signal directly.
type rowsErrIter struct{ err error }

func (it *rowsErrIter) Next() bool        { return false }
func (it *rowsErrIter) Scan(...any) error { return nil }
func (it *rowsErrIter) Err() error        { return it.err }
func (it *rowsErrIter) Close()            {}

// --- resolver unit tests --------------------------------------------------

func TestModelResolver_EmptyProjectID(t *testing.T) {
	r := NewModelResolver(ModelResolverConfig{DB: &fakeModelDB{}})
	if got := r.List(context.Background(), ""); len(got) != 0 {
		t.Fatalf("empty project id: got %d models, want 0", len(got))
	}
}

func TestModelResolver_NilDB(t *testing.T) {
	r := NewModelResolver(ModelResolverConfig{DB: nil})
	got := r.List(context.Background(), "42")
	if got == nil {
		t.Fatal("nil DB: List returned nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("nil DB: got %d models, want 0", len(got))
	}
}

func TestModelResolver_ResolvesAliasAndFallbackName(t *testing.T) {
	db := &fakeModelDB{rows: []fakeModelRow{
		{title: "gpt-4o-mine", data: []byte(`{"name":"gpt-4o"}`)}, // alias wins
		{title: "", data: []byte(`{"name":"claude-sonnet-4-5"}`)}, // fallback to data.name
	}}
	r := NewModelResolver(ModelResolverConfig{DB: db})
	got := r.List(context.Background(), "42")
	ids := modelIDs(got)
	want := []string{"gpt-4o-mine", "claude-sonnet-4-5"}
	if !equalStrs(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for _, mo := range got {
		if mo.Object != modelObjectType {
			t.Errorf("model %q object = %q, want %q", mo.ID, mo.Object, modelObjectType)
		}
		if mo.OwnedBy != modelsOwnedBy {
			t.Errorf("model %q owned_by = %q, want %q", mo.ID, mo.OwnedBy, modelsOwnedBy)
		}
	}
}

func TestModelResolver_SkipsUnusableAndDedups(t *testing.T) {
	db := &fakeModelDB{rows: []fakeModelRow{
		{title: "", data: nil},                   // no id at all → skipped
		{title: "", data: []byte(`{bad json`)},   // malformed data → skipped
		{title: "gpt-4o", data: []byte(`{}`)},    // kept
		{title: "gpt-4o", data: []byte(`{}`)},    // duplicate → first wins
		{title: "", data: []byte(`{"name":""}`)}, // empty name → skipped
	}}
	r := NewModelResolver(ModelResolverConfig{DB: db})
	got := r.List(context.Background(), "42")
	if ids := modelIDs(got); !equalStrs(ids, []string{"gpt-4o"}) {
		t.Fatalf("ids = %v, want [gpt-4o]", ids)
	}
}

func TestModelResolver_NonNumericProjectIDRejected(t *testing.T) {
	db := &fakeModelDB{rows: []fakeModelRow{{title: "x"}}}
	r := NewModelResolver(ModelResolverConfig{DB: db})
	// A non-numeric id is rejected before the query; with nothing cached the
	// resolver returns an empty set and never touches the DB.
	got := r.List(context.Background(), "42; DROP TABLE")
	if len(got) != 0 {
		t.Fatalf("non-numeric id: got %d models, want 0", len(got))
	}
	if db.calls != 0 {
		t.Fatalf("non-numeric id: DB queried %d times, want 0", db.calls)
	}
}

func TestModelResolver_CacheHitAvoidsSecondQuery(t *testing.T) {
	db := &fakeModelDB{rows: []fakeModelRow{{title: "gpt-4o"}}}
	clk := &fakeClock{t: time.Unix(1000, 0)}
	r := NewModelResolver(ModelResolverConfig{DB: db, CacheTTL: 60 * time.Second, Now: clk.now})

	_ = r.List(context.Background(), "42")
	_ = r.List(context.Background(), "42") // within TTL → cache hit
	if db.calls != queriesPerScope {
		t.Fatalf("DB queried %d times, want %d (second call should hit cache)", db.calls, queriesPerScope)
	}

	// Advance past the TTL → a re-query.
	clk.advance(61 * time.Second)
	_ = r.List(context.Background(), "42")
	if db.calls != 2*queriesPerScope {
		t.Fatalf("DB queried %d times after TTL expiry, want %d", db.calls, 2*queriesPerScope)
	}
}

func TestModelResolver_StaleServedOnError(t *testing.T) {
	db := &fakeModelDB{rows: []fakeModelRow{{title: "gpt-4o"}}}
	clk := &fakeClock{t: time.Unix(1000, 0)}
	r := NewModelResolver(ModelResolverConfig{DB: db, CacheTTL: 30 * time.Second, Now: clk.now})

	// Prime the cache.
	first := r.List(context.Background(), "42")
	if !equalStrs(modelIDs(first), []string{"gpt-4o"}) {
		t.Fatalf("prime: ids = %v", modelIDs(first))
	}

	// Expire the entry, then make the DB fail: the resolver must serve the
	// stale cached list rather than emptying the surface.
	clk.advance(31 * time.Second)
	db.err = errors.New("connection refused")
	got := r.List(context.Background(), "42")
	if !equalStrs(modelIDs(got), []string{"gpt-4o"}) {
		t.Fatalf("stale-on-error: ids = %v, want [gpt-4o]", modelIDs(got))
	}
}

func TestModelResolver_ErrorNoCacheReturnsEmpty(t *testing.T) {
	db := &fakeModelDB{err: errors.New("boom")}
	r := NewModelResolver(ModelResolverConfig{DB: db})
	got := r.List(context.Background(), "42")
	if got == nil || len(got) != 0 {
		t.Fatalf("error+no-cache: got %v, want empty non-nil slice", got)
	}
}

func TestModelResolver_RowsErrPropagatesToEmpty(t *testing.T) {
	// A DB whose Query succeeds but whose rows.Err() reports a deferred failure
	// must be treated as a query error (empty set, no cache).
	db := &erringRowsDB{err: errors.New("row stream broke")}
	r := NewModelResolver(ModelResolverConfig{DB: db})
	if got := r.List(context.Background(), "42"); len(got) != 0 {
		t.Fatalf("rows.Err path: got %d models, want 0", len(got))
	}
}

// erringRowsDB returns a rows iterator that fails on Err().
type erringRowsDB struct{ err error }

func (db *erringRowsDB) Query(context.Context, string, ...any) (modelRows, error) {
	return &rowsErrIter{err: db.err}, nil
}

func TestModelResolver_Get(t *testing.T) {
	db := &fakeModelDB{rows: []fakeModelRow{{title: "gpt-4o"}, {title: "openai/o1"}}}
	r := NewModelResolver(ModelResolverConfig{DB: db})

	if mo, ok := r.Get(context.Background(), "42", "openai/o1"); !ok || mo.ID != "openai/o1" {
		t.Fatalf("Get(openai/o1) = %+v, %v; want found", mo, ok)
	}
	if _, ok := r.Get(context.Background(), "42", "nope"); ok {
		t.Fatal("Get(nope) reported found, want not found")
	}
	// Both List and Get share the cache: 2 List/Get calls, one scope read.
	if db.calls != queriesPerScope {
		t.Fatalf("DB queried %d times, want %d (Get reuses List cache)", db.calls, queriesPerScope)
	}
}

// --- HTTP handler tests ---------------------------------------------------

func TestModels_ListEnvelope(t *testing.T) {
	db := &fakeModelDB{rows: []fakeModelRow{{title: "gpt-4o"}, {title: "claude-sonnet-4-5"}}}
	h := newModelsHandler(db)

	rec := getModels(t, h, "/llm/v1/models", "42")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var list modelsList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Object != modelsListType {
		t.Errorf("object = %q, want %q", list.Object, modelsListType)
	}
	if !equalStrs(modelIDs(list.Data), []string{"gpt-4o", "claude-sonnet-4-5"}) {
		t.Fatalf("ids = %v", modelIDs(list.Data))
	}
}

func TestModels_ListNilResolverEmpty(t *testing.T) {
	h := newTestHandler(&fakeRouter{}, nil, nil) // no WithModelResolver
	rec := getModels(t, h.route(), "/llm/v1/models", "42")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var list modelsList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Object != modelsListType || len(list.Data) != 0 {
		t.Fatalf("nil resolver: got object=%q data=%d, want list/0", list.Object, len(list.Data))
	}
}

func TestModel_SingleFoundAndNotFound(t *testing.T) {
	db := &fakeModelDB{rows: []fakeModelRow{{title: "gpt-4o"}}}
	h := newModelsHandler(db)

	// Found → 200 with a single model object.
	rec := getModels(t, h, "/llm/v1/models/gpt-4o", "42")
	if rec.Code != http.StatusOK {
		t.Fatalf("found: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var mo modelObject
	if err := json.Unmarshal(rec.Body.Bytes(), &mo); err != nil {
		t.Fatalf("decode model: %v", err)
	}
	if mo.ID != "gpt-4o" || mo.Object != modelObjectType {
		t.Fatalf("model = %+v, want id=gpt-4o object=model", mo)
	}

	// Absent → 404.
	rec = getModels(t, h, "/llm/v1/models/does-not-exist", "42")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("absent: status = %d, want 404", rec.Code)
	}
}

func TestModel_SlashInIDViaWildcard(t *testing.T) {
	db := &fakeModelDB{rows: []fakeModelRow{{title: "openai/gpt-4o"}}}
	h := newModelsHandler(db)
	// The wildcard route must carry the whole slash-containing id through.
	rec := getModels(t, h, "/llm/v1/models/openai/gpt-4o", "42")
	if rec.Code != http.StatusOK {
		t.Fatalf("slash id: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var mo modelObject
	_ = json.Unmarshal(rec.Body.Bytes(), &mo)
	if mo.ID != "openai/gpt-4o" {
		t.Fatalf("slash id: id = %q, want openai/gpt-4o", mo.ID)
	}
}

func TestModel_NilResolver404(t *testing.T) {
	h := newTestHandler(&fakeRouter{}, nil, nil)
	rec := getModels(t, h.route(), "/llm/v1/models/gpt-4o", "42")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nil resolver single: status = %d, want 404", rec.Code)
	}
}

func TestModels_InvalidSignature403(t *testing.T) {
	db := &fakeModelDB{rows: []fakeModelRow{{title: "gpt-4o"}}}
	resolver := NewModelResolver(ModelResolverConfig{DB: db})
	// A configured identity secret with a missing signature header → 403.
	h := newTestHandler(&fakeRouter{}, nil, []byte("secret"), WithModelResolver(resolver))

	req := httptest.NewRequest(http.MethodGet, "/llm/v1/models", nil)
	req.Header.Set(headerProjectID, "42") // no signature
	rec := httptest.NewRecorder()
	h.route().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad signature: status = %d, want 403", rec.Code)
	}
	if db.calls != 0 {
		t.Fatalf("bad signature: DB queried %d times, want 0", db.calls)
	}
}

func TestModels_ValidSignatureResolves(t *testing.T) {
	db := &fakeModelDB{rows: []fakeModelRow{{title: "gpt-4o"}}}
	resolver := NewModelResolver(ModelResolverConfig{DB: db})
	secret := []byte("secret")
	h := newTestHandler(&fakeRouter{}, nil, secret, WithModelResolver(resolver))

	id := identity{projectID: "42"}
	req := httptest.NewRequest(http.MethodGet, "/llm/v1/models", nil)
	req.Header.Set(headerProjectID, id.projectID)
	req.Header.Set(headerSignature, id.sign(secret))
	rec := httptest.NewRecorder()
	h.route().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid signature: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if db.calls != queriesPerScope {
		t.Fatalf("valid signature: DB queried %d times, want %d", db.calls, queriesPerScope)
	}
}

func TestModelNameFromPath(t *testing.T) {
	cases := map[string]string{
		"/llm/v1/models/gpt-4o":        "gpt-4o",
		"/llm/v1/models/openai/gpt-4o": "openai/gpt-4o",
		"/llm/v1/models/a%2Fb":         "a/b", // percent-escaped slash
		"/llm/v1/models/":              "",
		"/unrelated/path":              "",
	}
	for path, want := range cases {
		if got := modelNameFromPath(path); got != want {
			t.Errorf("modelNameFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}

// --- helpers --------------------------------------------------------------

// newModelsHandler builds a routed Handler with a resolver over db and no
// identity secret (signature verification disabled), returning the http.Handler.
func newModelsHandler(db modelRowQuerier) http.Handler {
	resolver := NewModelResolver(ModelResolverConfig{DB: db})
	h := newTestHandler(&fakeRouter{}, nil, nil, WithModelResolver(resolver))
	return h.route()
}

func getModels(t *testing.T, h http.Handler, path, projectID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if projectID != "" {
		req.Header.Set(headerProjectID, projectID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func modelIDs(ms []modelObject) []string {
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	return ids
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }
