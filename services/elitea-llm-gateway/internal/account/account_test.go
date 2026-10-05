package account

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// ─── test fakes ────────────────────────────────────────────────────────────

// fakeVault is an in-memory vaultDecryptor for tests that do not exercise real
// Fernet decryption (vault.go has dedicated tests for that).
type fakeVault struct {
	// secrets maps projectID → {{secret name}} → value.
	secrets map[string]map[string]string
	err     error
}

func (f *fakeVault) Resolve(_ context.Context, projectID, secretRef string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	// Self-contained {{secret.NAME}} parsing — does NOT call production parseSecretRef
	// so the fake is fully isolated from vault.go implementation details.
	const prefix = "{{secret."
	const suffix = "}}"
	if strings.HasPrefix(secretRef, prefix) && strings.HasSuffix(secretRef, suffix) {
		name := secretRef[len(prefix) : len(secretRef)-len(suffix)]
		if p, ok := f.secrets[projectID]; ok {
			if v, ok := p[name]; ok {
				return v, nil
			}
		}
		return "", errors.New("secret not found")
	}
	// Not a secret reference — return the literal value.
	return secretRef, nil
}

// fakeDB is an in-memory rowQuerier. queryRows keyed by nothing fancy: every
// Query returns the configured rows (tests use a single provider per case);
// queryErr forces Query to fail.
type fakeDB struct {
	// rows is the catch-all result: every Query returns it unless bySchema
	// carries an entry for the schema the query names. Each row is
	// {id string, title string, data []byte} with an optional 4th
	// {shared bool}; a 3-element row means shared = false.
	rows     [][]any
	queryErr error
	scanErr  error
	// bySchema routes rows per project schema ("7", "1", …) so a test can seed a
	// different result for the caller's project and for the public project.
	// A schema with no entry yields zero rows, NOT the catch-all.
	bySchema map[string][][]any
	// gotSQL records every statement the account issued, in order, so a test can
	// assert WHICH schemas were read and that the shared predicate was applied.
	gotSQL []string
	// ignoreSharedPredicate makes the fake return unpublished rows even when the
	// statement asks for `shared = true`. It simulates a query that lost its
	// predicate, so a test can prove the Go-side backstop still refuses the row.
	// Postgres is modelled faithfully by default (the predicate IS applied).
	ignoreSharedPredicate bool
	// keyRows/dataRows feed QueryRow (vault path) — not used by account tests.
	keyRow, dataRow []byte
	keyErr, dataErr error
}

func (d *fakeDB) Query(_ context.Context, sql string, _ ...any) (pgxRows, error) {
	d.gotSQL = append(d.gotSQL, sql)
	if d.queryErr != nil {
		return nil, d.queryErr
	}
	rows := d.rows
	if d.bySchema != nil {
		rows = d.bySchema[schemaProjectOf(sql)]
	}
	// Model Postgres: when the statement carries `shared = true`, only published
	// rows come back. Without this the fake would return rows the real database
	// never would, and an isolation test would prove nothing.
	if strings.Contains(sql, "shared = true") && !d.ignoreSharedPredicate {
		kept := make([][]any, 0, len(rows))
		for _, row := range rows {
			if len(row) > 3 && row[3].(bool) {
				kept = append(kept, row)
			}
		}
		rows = kept
	}
	return &fakeRows{rows: rows, scanErr: d.scanErr}, nil
}

// schemaProjectOf extracts the project id from the `FROM "p_<id>".configuration`
// clause of a statement, so the fake can serve per-project rows. Returns "" when
// the statement names no project schema.
func schemaProjectOf(sql string) string {
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

func (d *fakeDB) QueryRow(_ context.Context, sql string, _ ...any) pgxRow {
	if strings.Contains(sql, "secrets_key") {
		return &fakeRow{data: d.keyRow, err: d.keyErr}
	}
	return &fakeRow{data: d.dataRow, err: d.dataErr}
}

type fakeRow struct {
	data []byte
	err  error
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) > 0 {
		if p, ok := dest[0].(*[]byte); ok {
			*p = r.data
		}
	}
	return nil
}

type fakeRows struct {
	rows    [][]any
	i       int
	scanErr error
}

func (r *fakeRows) Next() bool { return r.i < len(r.rows) }
func (r *fakeRows) Err() error { return nil }
func (r *fakeRows) Close()     {}
func (r *fakeRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	row := r.rows[r.i]
	r.i++
	// row layout: id string, title string, data []byte, [shared bool], [type string]
	if p, ok := dest[0].(*string); ok {
		*p = row[0].(string)
	}
	if p, ok := dest[1].(*string); ok {
		*p = row[1].(string)
	}
	if p, ok := dest[2].(*[]byte); ok {
		*p = row[2].([]byte)
	}
	// A 3-element row means shared = false. Tests that exercise the shared scope
	// supply the 4th element explicitly.
	if len(dest) > 3 && len(row) > 3 {
		if p, ok := dest[3].(*bool); ok {
			*p = row[3].(bool)
		}
	}
	// An optional 5th element is the row's `type`. A row without it scans an
	// empty type, which no type-specific rule (the AI DIAL routes) matches.
	if len(dest) > 4 {
		if p, ok := dest[4].(*string); ok {
			*p = ""
			if len(row) > 4 {
				*p = row[4].(string)
			}
		}
	}
	return nil
}

// newTestAccount builds an EliteaAccount with the given DB and vault.
func newTestAccount(t *testing.T, db rowQuerier, vault vaultDecryptor, selfOrigins ...string) *EliteaAccount {
	t.Helper()
	a, err := New(Config{
		DB:                  db,
		Vault:               vault,
		ProviderConcurrency: 50,
		SelfOrigins:         selfOrigins,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func ctxWithProject(projectID string) context.Context {
	bc := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	bc.SetValue(schemas.BifrostContextKeyVirtualKey, projectID)
	return bc
}

// ─── New / config validation ───────────────────────────────────────────────

func TestNew_RequiresDBAndVault(t *testing.T) {
	if _, err := New(Config{Vault: &fakeVault{}}); err == nil {
		t.Fatal("expected error when DB is nil")
	}
	if _, err := New(Config{DB: &fakeDB{}}); err == nil {
		t.Fatal("expected error when Vault is nil")
	}
	if _, err := New(Config{DB: &fakeDB{}, Vault: &fakeVault{}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ─── GetConfiguredProviders / GetConfigForProvider ──────────────────────────

func TestGetConfiguredProviders(t *testing.T) {
	a := newTestAccount(t, &fakeDB{}, &fakeVault{})
	got, err := a.GetConfiguredProviders()
	if err != nil {
		t.Fatalf("GetConfiguredProviders: %v", err)
	}
	if len(got) != len(supportedProviders) {
		t.Fatalf("got %d providers, want %d", len(got), len(supportedProviders))
	}
	// Ensure the caller cannot mutate the package-level slice via the return.
	got[0] = schemas.ModelProvider("mutated")
	again, _ := a.GetConfiguredProviders()
	if again[0] == "mutated" {
		t.Fatal("GetConfiguredProviders returned a mutable view of the shared slice")
	}
}

func TestGetConfigForProvider_TunesConcurrency(t *testing.T) {
	a := newTestAccount(t, &fakeDB{}, &fakeVault{})
	cfg, err := a.GetConfigForProvider(schemas.OpenAI)
	if err != nil {
		t.Fatalf("GetConfigForProvider: %v", err)
	}
	if cfg.ConcurrencyAndBufferSize.Concurrency != 50 {
		t.Fatalf("Concurrency = %d, want 50", cfg.ConcurrencyAndBufferSize.Concurrency)
	}
}

// ─── GetKeysForProvider ─────────────────────────────────────────────────────

func TestGetKeysForProvider_NoProjectInContext(t *testing.T) {
	a := newTestAccount(t, &fakeDB{}, &fakeVault{})
	keys, err := a.GetKeysForProvider(context.Background(), schemas.OpenAI)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected zero keys with no project, got %d", len(keys))
	}
}

func TestGetKeysForProvider_ResolvesLiteralKey(t *testing.T) {
	db := &fakeDB{rows: [][]any{
		{"cfg-1", "OpenAI prod", []byte(`{"api_base":"https://api.openai.com/v1","api_key":"sk-literal"}`)},
	}}
	a := newTestAccount(t, db, &fakeVault{})

	keys, err := a.GetKeysForProvider(ctxWithProject("42"), schemas.OpenAI)
	if err != nil {
		t.Fatalf("GetKeysForProvider: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want 1", len(keys))
	}
	if keys[0].ID != "cfg-1" || keys[0].Name != "OpenAI prod" {
		t.Fatalf("unexpected key identity: %+v", keys[0])
	}
	if keys[0].Value.Val != "sk-literal" {
		t.Fatalf("key value = %q, want sk-literal", keys[0].Value.Val)
	}
	if !keys[0].Models.IsUnrestricted() {
		t.Fatalf("expected unrestricted model whitelist, got %v", keys[0].Models)
	}
}

func TestGetKeysForProvider_ResolvesSecretRef(t *testing.T) {
	db := &fakeDB{rows: [][]any{
		{"cfg-2", "OpenAI", []byte(`{"api_base":"https://api.openai.com/v1","api_key":"{{secret.OPENAI_KEY}}"}`)},
	}}
	vault := &fakeVault{secrets: map[string]map[string]string{
		"42": {"OPENAI_KEY": "sk-from-vault"},
	}}
	a := newTestAccount(t, db, vault)

	keys, err := a.GetKeysForProvider(ctxWithProject("42"), schemas.OpenAI)
	if err != nil {
		t.Fatalf("GetKeysForProvider: %v", err)
	}
	if len(keys) != 1 || keys[0].Value.Val != "sk-from-vault" {
		t.Fatalf("expected vault-resolved key, got %+v", keys)
	}
}

func TestGetKeysForProvider_LegacyAPITokenFallback(t *testing.T) {
	db := &fakeDB{rows: [][]any{
		{"cfg-3", "", []byte(`{"api_base":"https://api.openai.com/v1","api_token":"{{secret.TOK}}"}`)},
	}}
	vault := &fakeVault{secrets: map[string]map[string]string{"7": {"TOK": "tok-val"}}}
	a := newTestAccount(t, db, vault)

	keys, err := a.GetKeysForProvider(ctxWithProject("7"), schemas.OpenAI)
	if err != nil {
		t.Fatalf("GetKeysForProvider: %v", err)
	}
	if len(keys) != 1 || keys[0].Value.Val != "tok-val" {
		t.Fatalf("expected api_token fallback resolution, got %+v", keys)
	}
}

func TestGetKeysForProvider_OllamaCarriesURL(t *testing.T) {
	db := &fakeDB{rows: [][]any{
		{"cfg-o", "local", []byte(`{"api_base":"http://ollama:11434","api_key":""}`)},
	}}
	a := newTestAccount(t, db, &fakeVault{})

	keys, err := a.GetKeysForProvider(ctxWithProject("1"), schemas.Ollama)
	if err != nil {
		t.Fatalf("GetKeysForProvider: %v", err)
	}
	if len(keys) != 1 || keys[0].OllamaKeyConfig == nil {
		t.Fatalf("expected Ollama key config, got %+v", keys)
	}
	if keys[0].OllamaKeyConfig.URL.Val != "http://ollama:11434" {
		t.Fatalf("Ollama URL = %q", keys[0].OllamaKeyConfig.URL.Val)
	}
}

func TestGetKeysForProvider_AzureCarriesEndpoint(t *testing.T) {
	db := &fakeDB{rows: [][]any{
		{"cfg-az", "az", []byte(`{"api_base":"https://acme.openai.azure.com","api_key":"az-key"}`)},
	}}
	a := newTestAccount(t, db, &fakeVault{})

	keys, err := a.GetKeysForProvider(ctxWithProject("1"), schemas.Azure)
	if err != nil {
		t.Fatalf("GetKeysForProvider: %v", err)
	}
	if len(keys) != 1 || keys[0].AzureKeyConfig == nil {
		t.Fatalf("expected Azure key config, got %+v", keys)
	}
	if keys[0].AzureKeyConfig.Endpoint.Val != "https://acme.openai.azure.com" {
		t.Fatalf("Azure endpoint = %q", keys[0].AzureKeyConfig.Endpoint.Val)
	}
}

func TestGetKeysForProvider_UnmappedProviderYieldsNoKeys(t *testing.T) {
	// Cohere is in no providerConfigTypes entry → loadCredentials returns nil.
	db := &fakeDB{rows: [][]any{{"x", "y", []byte(`{}`)}}}
	a := newTestAccount(t, db, &fakeVault{})
	keys, err := a.GetKeysForProvider(ctxWithProject("1"), schemas.Cohere)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected zero keys for unmapped provider, got %d", len(keys))
	}
}

func TestGetKeysForProvider_SkipsMalformedRow(t *testing.T) {
	db := &fakeDB{rows: [][]any{
		{"bad", "", []byte(`{not json`)},
		{"good", "", []byte(`{"api_base":"https://api.openai.com/v1","api_key":"sk-ok"}`)},
	}}
	a := newTestAccount(t, db, &fakeVault{})
	keys, err := a.GetKeysForProvider(ctxWithProject("1"), schemas.OpenAI)
	if err != nil {
		t.Fatalf("GetKeysForProvider: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != "good" {
		t.Fatalf("expected only the well-formed row, got %+v", keys)
	}
}

// ─── self-referential guard ─────────────────────────────────────────────────

func TestGetKeysForProvider_RejectsSelfReferential(t *testing.T) {
	db := &fakeDB{rows: [][]any{
		{"loop", "", []byte(`{"api_base":"https://dev.elitea.ai/llm/v1","api_key":"sk"}`)},
	}}
	a := newTestAccount(t, db, &fakeVault{}, "https://dev.elitea.ai/llm/v1")

	_, err := a.GetKeysForProvider(ctxWithProject("1"), schemas.OpenAI)
	if err == nil {
		t.Fatal("expected self-referential rejection")
	}
	if !errors.Is(err, ErrSelfReferentialCredential) {
		t.Fatalf("error %v does not wrap ErrSelfReferentialCredential", err)
	}
	if !strings.Contains(err.Error(), SelfReferentialCredentialReason) {
		t.Fatalf("error %q missing reason code", err.Error())
	}
}

func TestGetKeysForProvider_SelfReferentialGuardBeforeVault(t *testing.T) {
	// The vault errors on any Resolve; the guard must fire before Resolve is
	// reached, so the returned error must be the guard, not the vault.
	db := &fakeDB{rows: [][]any{
		{"loop", "", []byte(`{"api_base":"http://pylon_main:8080/llm/v1","api_key":"{{secret.X}}"}`)},
	}}
	vault := &fakeVault{err: errors.New("vault must not be called")}
	a := newTestAccount(t, db, vault, "http://pylon_main:8080/llm/v1")

	_, err := a.GetKeysForProvider(ctxWithProject("1"), schemas.OpenAI)
	if !errors.Is(err, ErrSelfReferentialCredential) {
		t.Fatalf("expected guard error before vault, got %v", err)
	}
}

func TestIsSelfReferential_PrefixMatch(t *testing.T) {
	a := newTestAccount(t, &fakeDB{}, &fakeVault{}, "https://dev.elitea.ai/llm/v1")
	cases := map[string]bool{
		"https://dev.elitea.ai/llm/v1":       true,  // exact
		"https://dev.elitea.ai/llm/v1/":      true,  // trailing slash normalised
		"https://DEV.elitea.ai/llm/v1":       true,  // case-insensitive host
		"https://dev.elitea.ai/llm":          true,  // credential is a segment prefix of self
		"https://dev.elitea.ai/llm/v1/extra": true,  // self is a segment prefix of credential
		"https://api.openai.com/v1":          false, // unrelated
		"":                                   false, // empty
		"not a url":                          false, // unparsable
		// FIX 2: partial-segment prefix must NOT match.
		// "/llm/v" is NOT a segment prefix of "/llm/v1".
		"https://dev.elitea.ai/llm/v": false,
		// FIX 3: uppercase path must still be caught (case-insensitive comparison).
		"https://dev.elitea.ai/LLM/V1":  true,
		"https://dev.elitea.ai/Llm/V1/": true,
	}
	for base, want := range cases {
		if got := a.isSelfReferential(base); got != want {
			t.Errorf("isSelfReferential(%q) = %v, want %v", base, got, want)
		}
	}
}

func TestIsSelfReferential_SegmentBoundaryEvasion(t *testing.T) {
	// FIX 2 regression test: a credential whose normalised path is a partial
	// segment of a self-origin path must not be caught as self-referential.
	// "/llm/v" shares a raw string prefix with "/llm/v1" but does NOT share a
	// segment boundary — only full-segment prefixes are loops.
	a := newTestAccount(t, &fakeDB{}, &fakeVault{}, "https://dev.elitea.ai/llm/v1")
	if a.isSelfReferential("https://dev.elitea.ai/llm/v") {
		t.Error("partial-segment credential /llm/v must not match self /llm/v1")
	}
	if a.isSelfReferential("https://dev.elitea.ai/llm/v10") {
		t.Error("partial-segment credential /llm/v10 must not match self /llm/v1 — different segment")
	}
	// But a full extra segment must still match.
	if !a.isSelfReferential("https://dev.elitea.ai/llm/v1/models") {
		t.Error("credential /llm/v1/models should match self /llm/v1 (self is segment prefix)")
	}
}

func TestIsSelfReferential_UppercasePathBypass(t *testing.T) {
	// FIX 3: a credential with an uppercase path must still be caught.
	a := newTestAccount(t, &fakeDB{}, &fakeVault{}, "https://dev.elitea.ai/llm/v1")
	for _, base := range []string{
		"https://dev.elitea.ai/LLM/V1",
		"https://dev.elitea.ai/Llm/V1",
		"HTTPS://DEV.ELITEA.AI/LLM/V1",
	} {
		if !a.isSelfReferential(base) {
			t.Errorf("uppercase-path credential %q must be caught as self-referential", base)
		}
	}
}

func TestNormaliseOrigin(t *testing.T) {
	cases := map[string]string{
		"https://Dev.Elitea.AI/llm/v1/": "https://dev.elitea.ai/llm/v1",
		// Path case is preserved in the normalised form; isSelfReferential applies
		// case-insensitive comparison at match time (FIX 3).
		"HTTP://Host:8080/Path":  "http://host:8080/Path",
		"HTTP://Host:8080/PATH/": "http://host:8080/PATH",
		"":                       "",
		"::::":                   "",
		// Fix #4: trailing dot stripped from FQDN host.
		"https://host./llm/v1": "https://host/llm/v1",
		"https://HOST./LLM/V1": "https://host/LLM/V1",
		// Fix #4: explicit default ports stripped.
		"https://host:443/llm/v1": "https://host/llm/v1",
		"http://host:80/llm/v1":   "http://host/llm/v1",
		// Non-default ports must be retained.
		"https://host:8443/llm/v1": "https://host:8443/llm/v1",
		"http://host:8080/llm/v1":  "http://host:8080/llm/v1",
	}
	for in, want := range cases {
		if got := normaliseOrigin(in); got != want {
			t.Errorf("normaliseOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIsSelfReferential_DefaultPortBypass asserts Fix #4: a credential with an
// explicit default port (https:443 / http:80) must be caught as self-referential
// when the self-origin is registered without the port.
func TestIsSelfReferential_DefaultPortBypass(t *testing.T) {
	a := newTestAccount(t, &fakeDB{}, &fakeVault{}, "https://dev.elitea.ai/llm/v1")
	cases := []struct {
		cred string
		want bool
	}{
		{"https://dev.elitea.ai:443/llm/v1", true},   // explicit :443 == no port for HTTPS
		{"http://dev.elitea.ai:80/llm/v1", false},    // scheme mismatch — http vs registered https
		{"https://dev.elitea.ai:8443/llm/v1", false}, // non-default port is a distinct origin
	}
	for _, tc := range cases {
		if got := a.isSelfReferential(tc.cred); got != tc.want {
			t.Errorf("isSelfReferential(%q) = %v, want %v", tc.cred, got, tc.want)
		}
	}
}

// TestIsSelfReferential_TrailingDotBypass asserts Fix #4: a credential with a
// trailing dot on the FQDN must be caught as self-referential.
func TestIsSelfReferential_TrailingDotBypass(t *testing.T) {
	a := newTestAccount(t, &fakeDB{}, &fakeVault{}, "https://dev.elitea.ai/llm/v1")
	cases := []struct {
		cred string
		want bool
	}{
		{"https://dev.elitea.ai./llm/v1", true},    // trailing dot == same host
		{"https://DEV.ELITEA.AI./LLM/V1", true},    // trailing dot + uppercase host + uppercase path
		{"https://dev.elitea.ai:443/llm/v1", true}, // explicit default port
	}
	for _, tc := range cases {
		if got := a.isSelfReferential(tc.cred); got != tc.want {
			t.Errorf("isSelfReferential(%q) = %v, want %v", tc.cred, got, tc.want)
		}
	}
}

// ─── error propagation ──────────────────────────────────────────────────────

func TestGetKeysForProvider_QueryError(t *testing.T) {
	db := &fakeDB{queryErr: errors.New("db down")}
	a := newTestAccount(t, db, &fakeVault{})
	if _, err := a.GetKeysForProvider(ctxWithProject("1"), schemas.OpenAI); err == nil {
		t.Fatal("expected query error to propagate")
	}
}

func TestGetKeysForProvider_VaultError(t *testing.T) {
	db := &fakeDB{rows: [][]any{
		{"c", "", []byte(`{"api_base":"https://api.openai.com/v1","api_key":"{{secret.MISSING}}"}`)},
	}}
	vault := &fakeVault{err: errors.New("vault boom")}
	a := newTestAccount(t, db, vault)
	if _, err := a.GetKeysForProvider(ctxWithProject("1"), schemas.OpenAI); err == nil {
		t.Fatal("expected vault error to propagate")
	}
}

func TestGetKeysForProvider_InvalidProjectID(t *testing.T) {
	a := newTestAccount(t, &fakeDB{}, &fakeVault{})
	if _, err := a.GetKeysForProvider(ctxWithProject("1; DROP TABLE"), schemas.OpenAI); err == nil {
		t.Fatal("expected invalid project id to be rejected")
	}
}

func TestProjectIDFromContext(t *testing.T) {
	if got := projectIDFromContext(context.TODO()); got != "" {
		t.Fatalf("nil ctx: got %q", got)
	}
	if got := projectIDFromContext(context.Background()); got != "" {
		t.Fatalf("no value: got %q", got)
	}
	if got := projectIDFromContext(ctxWithProject(" 99 ")); got != "99" {
		t.Fatalf("trimmed value: got %q", got)
	}
}

// TestBuildKey_VLLM asserts that the adapter removes the /v1 suffix that
// Bifrost adds itself and leaves the key-level model filter empty.
func TestBuildKey_VLLM(t *testing.T) {
	k, err := buildKey(schemas.VLLM, credential{
		configID: "c1", name: "local-vllm", apiBase: "http://192.168.0.1:8000/v1",
	}, "sk-anything", "", "")
	if err != nil {
		t.Fatalf("buildKey: %v", err)
	}
	if k.VLLMKeyConfig == nil {
		t.Fatal("VLLMKeyConfig = nil, want URL set from api_base")
	}
	if got := k.VLLMKeyConfig.URL.GetValue(); got != "http://192.168.0.1:8000" {
		t.Errorf("VLLMKeyConfig.URL = %q, want normalized api_base", got)
	}
	if k.VLLMKeyConfig.ModelName != "" {
		t.Errorf("ModelName = %q, want empty (no key-level filter)", k.VLLMKeyConfig.ModelName)
	}
}

func TestBifrostVLLMBaseURL(t *testing.T) {
	tests := map[string]string{
		"http://vllm:8000":               "http://vllm:8000",
		"http://vllm:8000/":              "http://vllm:8000",
		"http://vllm:8000/v1":            "http://vllm:8000",
		"http://gateway:8080/llm/v1/":    "http://gateway:8080/llm",
		"http://gateway:8080/service/v2": "http://gateway:8080/service/v2",
	}
	for input, want := range tests {
		if got := bifrostVLLMBaseURL(input); got != want {
			t.Errorf("bifrostVLLMBaseURL(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestProviderConfigTypes_VLLM asserts the vllm provider resolves the "vllm"
// configuration type and is in the configured-provider set.
func TestProviderConfigTypes_VLLM(t *testing.T) {
	if got := providerConfigTypes[schemas.VLLM]; len(got) != 1 || got[0] != "vllm" {
		t.Fatalf("providerConfigTypes[VLLM] = %v, want [vllm]", got)
	}
	a, err := New(Config{DB: &fakeDB{}, Vault: &fakeVault{}})
	if err != nil {
		t.Fatal(err)
	}
	provs, _ := a.GetConfiguredProviders()
	found := false
	for _, p := range provs {
		if p == schemas.VLLM {
			found = true
		}
	}
	if !found {
		t.Error("GetConfiguredProviders() missing schemas.VLLM")
	}
}

// TestBuildKey_VLLM_UseAnthropicEndpoints asserts the credential's
// use_anthropic_endpoints flag reaches schemas.Key so bifrost routes the
// request to the upstream's Anthropic-compatible /v1/messages surface
// (BFF.9a: an OpenAI-compatible gateway that also serves the Anthropic dialect).
func TestBuildKey_VLLM_UseAnthropicEndpoints(t *testing.T) {
	off, err := buildKey(schemas.VLLM, credential{configID: "c1", apiBase: "https://up.example"}, "sk", "", "")
	if err != nil {
		t.Fatalf("buildKey: %v", err)
	}
	if off.UseAnthropicEndpoints != nil {
		t.Errorf("UseAnthropicEndpoints = %v, want nil when the flag is unset", *off.UseAnthropicEndpoints)
	}
	on, err := buildKey(schemas.VLLM, credential{configID: "c2", apiBase: "https://up.example", useAnthropicEndpoints: true}, "sk", "", "")
	if err != nil {
		t.Fatalf("buildKey: %v", err)
	}
	if on.UseAnthropicEndpoints == nil || !*on.UseAnthropicEndpoints {
		t.Fatal("UseAnthropicEndpoints must be true when the credential sets use_anthropic_endpoints")
	}
}
