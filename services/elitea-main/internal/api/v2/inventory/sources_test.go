package inventory_test

// Inventory's source expansion (ADR-0023 H4c I2).
//
// THE DECISIVE ASSERTIONS IN THIS FILE COUNT CALLS, NOT ERROR STRINGS. Every
// claim the facade makes is a claim about ORDER — admitted before typed,
// typed before egress-checked, egress-checked before decrypted, decrypted
// before minted — and an assertion on an error message would pass just as
// well if the steps ran in the wrong order and the refusal came afterwards.
// So the settings resolver counts its CLAIM-mode calls (the only ones that
// open a vault) and the minter records what it issued.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/inventory"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/material"
)

// ---------------------------------------------------------------------------
// doubles
// ---------------------------------------------------------------------------

type fakeToolkits struct {
	rows map[int32]repos.CurrentToolkit
}

func (f *fakeToolkits) Get(
	_ context.Context, projectID, toolkitID int32,
) (repos.CurrentToolkit, error) {
	if projectID != testProject {
		return repos.CurrentToolkit{}, repos.ErrCurrentToolkitNotFound
	}
	row, ok := f.rows[toolkitID]
	if !ok {
		return repos.CurrentToolkit{}, repos.ErrCurrentToolkitNotFound
	}
	return row, nil
}

// countingSettings stands in for the resolver. `decrypts` counts CLAIM-mode
// calls and is the assertion surface for the check-before-decrypt order:
// reference mode redeems no secret, so only this number can rise.
type countingSettings struct {
	mu       sync.Mutex
	decrypts int
	sealed   int
}

func (s *countingSettings) Resolve(
	_ context.Context, request configurationapp.CurrentToolkitSettingsRequest,
) (map[string]any, error) {
	s.mu.Lock()
	claim := request.Mode == configurationapp.CurrentToolkitSettingsClaimMode
	if claim {
		s.decrypts++
	} else {
		s.sealed++
	}
	s.mu.Unlock()

	expanded := map[string]any{}
	for key, value := range request.Settings {
		expanded[key] = value
	}
	host := material.Text(request.Settings["__host"])
	switch request.ToolkitType {
	case "github":
		token := "{{secret.github_pat}}"
		if claim {
			token = "ghp_decrypted_secret"
		}
		expanded["github_configuration"] = map[string]any{
			"base_url": host, "access_token": token}
		// A field the projection must DROP: the provider has no business
		// receiving the project's vector-store connection string.
		expanded["pgvector_configuration"] = map[string]any{
			"connection_string": "postgres://user:pass@db/elitea"}
	case "ado_repos":
		token := "{{secret.ado_pat}}"
		if claim {
			token = "ado_decrypted_secret"
		}
		expanded["ado_configuration"] = map[string]any{
			"organization_url": host, "token": token}
	}
	return expanded, nil
}

func (s *countingSettings) counts() (sealed, decrypts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sealed, s.decrypts
}

type recordingMinter struct {
	mu      sync.Mutex
	minted  []string
	revoked []string
	counter int
}

func (m *recordingMinter) Mint(
	_ context.Context, _, _ int64, _ string, lifetime time.Duration,
) (material.Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counter++
	uuid := "token-" + string(rune('a'+m.counter-1))
	m.minted = append(m.minted, uuid)
	return material.Grant{
		Bearer: "bearer-for-" + uuid, Expires: time.Now().Add(lifetime), UUID: uuid}, nil
}

func (m *recordingMinter) Revoke(_ context.Context, _ int64, tokenUUID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revoked = append(m.revoked, tokenUUID)
	return nil
}

func (m *recordingMinter) snapshot() (minted, revoked []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.minted...), append([]string(nil), m.revoked...)
}

// ---------------------------------------------------------------------------
// rows
// ---------------------------------------------------------------------------

const (
	testProject  = int32(42)
	inventoryRow = int32(7)
	githubRow    = int32(101)
	gitlabRow    = int32(102)
	adoRow       = int32(103)
	unlistedRow  = int32(999)

	fixtures = "../../../../../../conformance/provider/fixtures/inventory/invoke"
)

func name(value string) *string { return &value }

// toolkits is the project's rows: one Inventory toolkit naming three sources,
// the three sources themselves, and a fourth toolkit it does NOT name.
func toolkits() *fakeToolkits {
	return &fakeToolkits{rows: map[int32]repos.CurrentToolkit{
		inventoryRow: {ID: inventoryRow, Type: "inventory", Name: name("kg"),
			Settings: decode(`{
				"bucket": "kg",
				"llm_model": "gpt-4o",
				"sources": [101, 102, 103],
				"source_configs": {"101": {"file_patterns": "**/*.go", "branch": "develop"}}
			}`)},
		githubRow: {ID: githubRow, Type: "github", Name: name("acme-widgets"),
			Settings: decode(`{
				"github_configuration": {"elitea_title": "gh", "private": false},
				"repository": "acme/widgets",
				"active_branch": "main",
				"base_branch": "main",
				"__host": "https://api.github.com"
			}`)},
		gitlabRow: {ID: gitlabRow, Type: "gitlab", Name: name("mirror"),
			Settings: decode(`{"url": "https://gitlab.com"}`)},
		adoRow: {ID: adoRow, Type: "ado_repos", Name: name("internal"),
			Settings: decode(`{
				"ado_configuration": {"elitea_title": "ado", "private": false},
				"project": "Platform",
				"repository_id": "core",
				"__host": "https://dev.azure.example.invalid"
			}`)},
		unlistedRow: {ID: unlistedRow, Type: "github", Name: name("not-a-source"),
			Settings: decode(`{"repository": "acme/secret", "__host": "https://api.github.com"}`)},
	}}
}

// decode parses with UseNumber, which is how the toolkit repository decodes a
// settings column — integral ids must not round through float64.
func decode(raw string) any {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		panic(err)
	}
	return value
}

func config() inventory.SourcesConfig {
	return inventory.SourcesConfig{
		GitEgress: material.ParseGitEgress(
			"github.com, *.github.com", inventory.GitAllowlistEnv),
		SourceTypes:      inventory.DefaultSourceTypes,
		CallbackBaseURL:  "https://elitea.example",
		CallbackTokenTTL: time.Hour,
	}
}

func sources(
	t *testing.T, settings material.SettingsResolver, minter material.Minter,
	cfg inventory.SourcesConfig,
) *inventory.Sources {
	t.Helper()
	built, err := inventory.NewSources(toolkits(), settings, minter, cfg)
	if err != nil {
		t.Fatalf("compose the source expander: %v", err)
	}
	return built
}

// invokeBody is the provider's envelope: the invoking toolkit's own
// configuration, and the tool's arguments. The `sources` it carries names a
// toolkit the invoking row does NOT — the gate must read the row.
func invokeBody(sourceID int32, extraArgs string) string {
	return `{"configuration":{"application_id":7,"project_id":42,"parameters":{` +
		`"bucket":"kg","llm_model":"gpt-4o","sources":[999],` +
		`"source_configs":{"999":{"branch":"attacker"}}}},` +
		`"parameters":{"toolkit_id":` + strconv.Itoa(int(sourceID)) + extraArgs + `}}`
}

func rewrite(t *testing.T, s *inventory.Sources, body string) (map[string]any, error) {
	t.Helper()
	raw, _, err := s.Rewrite(context.Background(), strings.NewReader(body), 42, 11)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	return decoded, nil
}

func parametersOf(t *testing.T, decoded map[string]any) map[string]any {
	t.Helper()
	configuration, ok := decoded["configuration"].(map[string]any)
	if !ok {
		t.Fatal("the rewritten body has no configuration")
	}
	parameters, ok := configuration["parameters"].(map[string]any)
	if !ok {
		t.Fatal("the rewritten body has no configuration.parameters")
	}
	return parameters
}

func sourceOf(t *testing.T, decoded map[string]any) map[string]any {
	t.Helper()
	parameters := parametersOf(t, decoded)
	source, ok := parameters["source"].(map[string]any)
	if !ok {
		t.Fatalf("the rewritten body has no expanded source: %v", parameters)
	}
	return source
}

// ---------------------------------------------------------------------------
// the gate
// ---------------------------------------------------------------------------

func TestASourceOutsideTheToolkitsOwnListIsRefusedWithoutTouchingTheVault(t *testing.T) {
	settings := &countingSettings{}
	minter := &recordingMinter{}
	_, err := rewrite(t, sources(t, settings, minter, config()), invokeBody(unlistedRow, ""))
	if !errors.Is(err, material.ErrSourceNotAdmitted) {
		t.Fatalf("want ErrSourceNotAdmitted, got %v", err)
	}
	if sealed, decrypts := settings.counts(); sealed != 0 || decrypts != 0 {
		t.Errorf("the resolver ran for a source that was never admitted: "+
			"%d sealed, %d claim", sealed, decrypts)
	}
	if minted, _ := minter.snapshot(); len(minted) != 0 {
		t.Errorf("a callback token was minted for a refused source: %v", minted)
	}
}

func TestASourceOutsideTheListAnswers403(t *testing.T) {
	settings := &countingSettings{}
	response, bodies := invoke(t, settings, &recordingMinter{},
		"run_ingestion", invokeBody(unlistedRow, ""), http.StatusOK)
	if response.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", response.Code, response.Body.String())
	}
	if len(*bodies) != 0 {
		t.Error("a refused invocation reached the provider")
	}
	if _, decrypts := settings.counts(); decrypts != 0 {
		t.Errorf("a refused invocation opened the vault %d time(s)", decrypts)
	}
}

// TestTheClientsOwnSourcesListIsNotTheGate is the same refusal with the BODY
// saying the source is allowed. The gate reads the row.
func TestTheClientsOwnSourcesListIsNotTheGate(t *testing.T) {
	body := `{"configuration":{"application_id":7,"parameters":{"sources":[999]}},` +
		`"parameters":{"toolkit_id":999}}`
	_, err := rewrite(t, sources(t, &countingSettings{}, &recordingMinter{}, config()), body)
	if !errors.Is(err, material.ErrSourceNotAdmitted) {
		t.Fatalf("the body's own sources list admitted a source: %v", err)
	}
}

func TestATypeOutsideTheAllowlistIsRefusedWithoutDecrypting(t *testing.T) {
	settings := &countingSettings{}
	_, err := rewrite(t, sources(t, settings, &recordingMinter{}, config()),
		invokeBody(gitlabRow, ""))
	if !errors.Is(err, material.ErrSourceRefused) {
		t.Fatalf("want ErrSourceRefused for a gitlab source, got %v", err)
	}
	if !strings.Contains(err.Error(), "gitlab") {
		t.Errorf("the refusal must name the type it refused: %v", err)
	}
	if sealed, decrypts := settings.counts(); sealed != 0 || decrypts != 0 {
		t.Errorf("a refused type reached the resolver: %d sealed, %d claim", sealed, decrypts)
	}
}

// TestAnAllowedTypeWithNoProjectionCannotBeConfiguredIn pins the asymmetry:
// SourceTypes SUBTRACTS from SourceKinds and can never add to it.
func TestAnAllowedTypeWithNoProjectionCannotBeConfiguredIn(t *testing.T) {
	cfg := config()
	cfg.SourceTypes = []string{"github", "ado_repos", "gitlab"}
	built := sources(t, &countingSettings{}, &recordingMinter{}, cfg)
	if _, err := rewrite(t, built, invokeBody(gitlabRow, "")); !errors.Is(err, material.ErrSourceRefused) {
		t.Fatalf("configuration admitted a type with no field projection: %v", err)
	}
}

func TestAHostOffTheAllowlistIsRefusedBeforeTheDecrypt(t *testing.T) {
	settings := &countingSettings{}
	minter := &recordingMinter{}
	// ado_repos is an allowed TYPE whose host is not an allowed HOST, which
	// is what makes this a test of the egress step rather than the type step.
	_, err := rewrite(t, sources(t, settings, minter, config()), invokeBody(adoRow, ""))
	if !errors.Is(err, material.ErrEgressRefused) {
		t.Fatalf("want ErrEgressRefused, got %v", err)
	}
	sealed, decrypts := settings.counts()
	if sealed != 1 {
		t.Errorf("the host must be read from SEALED settings exactly once, got %d", sealed)
	}
	if decrypts != 0 {
		t.Fatalf("THE ORDER IS BROKEN: the vault opened %d time(s) for a host "+
			"the allowlist then refused", decrypts)
	}
	if minted, _ := minter.snapshot(); len(minted) != 0 {
		t.Errorf("a callback token was minted for a refused host: %v", minted)
	}
}

func TestAnUnsetAllowlistRefusesEveryHost(t *testing.T) {
	cfg := config()
	cfg.GitEgress = material.ParseGitEgress("", inventory.GitAllowlistEnv)
	settings := &countingSettings{}
	_, err := rewrite(t, sources(t, settings, &recordingMinter{}, cfg), invokeBody(githubRow, ""))
	if !errors.Is(err, material.ErrEgressRefused) {
		t.Fatalf("an unset allowlist permitted a clone: %v", err)
	}
	if !strings.Contains(err.Error(), inventory.GitAllowlistEnv) {
		t.Errorf("the refusal must name the variable to set: %v", err)
	}
	if _, decrypts := settings.counts(); decrypts != 0 {
		t.Errorf("a fail-closed refusal opened the vault %d time(s)", decrypts)
	}
}

func TestAnExpandedSourceObjectIsRefusedRatherThanMerged(t *testing.T) {
	settings := &countingSettings{}
	body := `{"configuration":{"application_id":7},"parameters":{"toolkit_id":` +
		`{"github_configuration":{"access_token":"ghp_attacker"}}}}`
	_, err := rewrite(t, sources(t, settings, &recordingMinter{}, config()), body)
	if !errors.Is(err, material.ErrRejected) {
		t.Fatalf("want a rejection for an expanded toolkit_id, got %v", err)
	}
	if strings.Contains(err.Error(), "ghp_attacker") {
		t.Errorf("the refusal echoed the credential the client pushed: %v", err)
	}
	if sealed, decrypts := settings.counts(); sealed != 0 || decrypts != 0 {
		t.Error("a client-pushed credential reached the resolver")
	}
}

func TestAnExpandedApplicationIdIsRefused(t *testing.T) {
	body := `{"configuration":{"application_id":{"sources":[999]}},"parameters":{"toolkit_id":999}}`
	_, err := rewrite(t, sources(t, &countingSettings{}, &recordingMinter{}, config()), body)
	if !errors.Is(err, material.ErrRejected) {
		t.Fatalf("want a rejection for an expanded application_id, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// the expansion
// ---------------------------------------------------------------------------

func TestOnlyTheFieldsTheSDKLoaderNeedsCross(t *testing.T) {
	settings := &countingSettings{}
	decoded, err := rewrite(t, sources(t, settings, &recordingMinter{}, config()),
		invokeBody(githubRow, ""))
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	source := sourceOf(t, decoded)
	credentials, ok := source["github_configuration"].(map[string]any)
	if !ok {
		t.Fatalf("no github_configuration in the projected source: %v", source)
	}
	if credentials["access_token"] != "ghp_decrypted_secret" {
		t.Errorf("the credential did not cross decrypted: %v", credentials["access_token"])
	}
	if source["repository"] != "acme/widgets" || source["type"] != "github" {
		t.Errorf("the source's own settings did not cross: %v", source)
	}
	// THE POINT OF AN ENUMERATION rather than a filter.
	if _, present := source["pgvector_configuration"]; present {
		t.Error("the project's vector-store credential crossed to the provider")
	}
	if _, present := source["__host"]; present {
		t.Error("a field nobody enumerated crossed to the provider")
	}
	if _, decrypts := settings.counts(); decrypts != 1 {
		t.Errorf("want exactly one claim-mode resolve, got %d", decrypts)
	}
}

func TestSourceConfigsMergeWithTheCallersBranchWinning(t *testing.T) {
	built := sources(t, &countingSettings{}, &recordingMinter{}, config())

	// Nothing from the caller: the stored per-source configuration decides.
	decoded, err := rewrite(t, built, invokeBody(githubRow, ""))
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	source := sourceOf(t, decoded)
	if source["branch"] != "develop" || source["file_patterns"] != "**/*.go" {
		t.Errorf("the stored source_configs did not apply: %v", source)
	}

	// The caller names a branch: THE CALLER WINS, which is legacy's rule
	// (`if source_config.get("branch") and not branch`).
	decoded, err = rewrite(t, built,
		invokeBody(githubRow, `,"branch":"release","exclude_patterns":"**/vendor/**"`))
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	source = sourceOf(t, decoded)
	if source["branch"] != "release" || source["active_branch"] != "release" {
		t.Errorf("the caller's branch did not win: %v", source)
	}
	// The PATTERNS are the other way round, and deliberately so.
	if source["file_patterns"] != "**/*.go" {
		t.Errorf("the stored file_patterns must beat the caller's: %v", source)
	}
	if source["exclude_patterns"] != "**/vendor/**" {
		t.Errorf("a pattern the stored config does not set must fall back to the caller: %v", source)
	}
	// The attacker's own source_configs, carried in the BODY, decide nothing.
	if source["branch"] == "attacker" {
		t.Error("the client's own source_configs were honoured")
	}
}

// ---------------------------------------------------------------------------
// the grant
// ---------------------------------------------------------------------------

func TestTheGrantIsMintedOnlyAfterTheSourceResolves(t *testing.T) {
	minter := &recordingMinter{}
	decoded, err := rewrite(t, sources(t, &countingSettings{}, minter, config()),
		invokeBody(githubRow, ""))
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	minted, revoked := minter.snapshot()
	if len(minted) != 1 || len(revoked) != 0 {
		t.Fatalf("want one grant and no revocation, got %v / %v", minted, revoked)
	}
	block, ok := parametersOf(t, decoded)["llm_settings"].(map[string]any)
	if !ok {
		t.Fatalf("no llm_settings block: %v", parametersOf(t, decoded))
	}
	if block["api_key"] != "bearer-for-token-a" ||
		block["api_base"] != "https://elitea.example/llm/v1" ||
		block["organization"] != "42" {
		t.Errorf("the callback block is not the minted grant's: %v", block)
	}
}

// TestARefusedHopGivesTheGrantBack drives the composed route, which is where a
// minted grant is actually revoked. Two of the invoking toolkit's three
// sources would resolve; the one this invocation names does not survive the
// hop, and the whole invocation is refused.
func TestARefusedHopGivesTheGrantBack(t *testing.T) {
	minter := &recordingMinter{}
	response, _ := invoke(t, &countingSettings{}, minter,
		"run_ingestion", invokeBody(githubRow, ""), http.StatusInternalServerError)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("want the provider's own status, got %d", response.Code)
	}
	minted, revoked := minter.snapshot()
	if len(minted) != 1 || len(revoked) != 1 || minted[0] != revoked[0] {
		t.Errorf("a refused hop must give its grant back: minted %v, revoked %v", minted, revoked)
	}
}

func TestAnAcceptedInvocationKeepsItsGrant(t *testing.T) {
	minter := &recordingMinter{}
	response, bodies := invoke(t, &countingSettings{}, minter,
		"run_ingestion", invokeBody(githubRow, ""), http.StatusOK)
	if response.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", response.Code, response.Body.String())
	}
	if _, revoked := minter.snapshot(); len(revoked) != 0 {
		t.Errorf("an accepted invocation revoked its own grant: %v", revoked)
	}
	if len(*bodies) != 1 || !strings.Contains((*bodies)[0], "ghp_decrypted_secret") {
		t.Fatalf("the provider did not receive the expanded source: %v", *bodies)
	}
	// The caller's own body named source 999 and its own llm_settings; neither
	// crossed the hop.
	if strings.Contains((*bodies)[0], "sk-attacker") ||
		!strings.Contains((*bodies)[0], "bearer-for-token-a") {
		t.Errorf("the callback block is not this facade's: %v", *bodies)
	}
}

// TestAToolThatNamesNoSourceMintsNothing pins the tool filter: eight of the
// eleven tools read the graph ingestion built, and a grant for one of those is
// a credential issued for work that asks for none.
func TestAToolThatNamesNoSourceMintsNothing(t *testing.T) {
	settings := &countingSettings{}
	minter := &recordingMinter{}
	response, bodies := invoke(t, settings, minter,
		"list_graphs", `{"parameters":{"output_format":"json"}}`, http.StatusOK)
	if response.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", response.Code, response.Body.String())
	}
	if len(*bodies) != 1 || strings.Contains((*bodies)[0], "llm_settings") {
		t.Errorf("a graph-reading tool had its body rewritten: %v", *bodies)
	}
	if minted, _ := minter.snapshot(); len(minted) != 0 {
		t.Errorf("a graph-reading tool minted a callback token: %v", minted)
	}
	if _, decrypts := settings.counts(); decrypts != 0 {
		t.Errorf("a graph-reading tool opened the vault %d time(s)", decrypts)
	}
}

// ---------------------------------------------------------------------------
// the golden fixture
// ---------------------------------------------------------------------------

// TestTheForwardedBodyMatchesTheRecordedFixture pins the exact bytes the
// provider receives. The fixture is the readable form of the contract this
// facade holds with the Inventory engine; a change to either must change it.
func TestTheForwardedBodyMatchesTheRecordedFixture(t *testing.T) {
	in, err := os.ReadFile(filepath.Join(fixtures, "run_ingestion.in.json"))
	if err != nil {
		t.Fatalf("read the invoke fixture: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(fixtures, "run_ingestion.out.json"))
	if err != nil {
		t.Fatalf("read the forwarded fixture: %v", err)
	}
	built := sources(t, &countingSettings{}, &recordingMinter{}, config())
	got, _, err := built.Rewrite(context.Background(), bytes.NewReader(in), 42, 11)
	if err != nil {
		t.Fatalf("rewrite the fixture: %v", err)
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, got, "", "  "); err != nil {
		t.Fatalf("indent: %v", err)
	}
	indented.WriteByte('\n')
	if !bytes.Equal(indented.Bytes(), want) {
		t.Errorf("the forwarded body drifted from the fixture.\n--- want ---\n%s\n--- got ---\n%s",
			want, indented.Bytes())
	}
}

// TestTheDescriptorStillNamesTheToolsThisFacadeExpands is the pin between the
// recorded descriptor and the tool list above. A tool renamed in the provider
// and not here would silently stop being rewritten — it would forward an
// unexpanded id, and the provider would fail on a payload the caller wrote
// correctly.
func TestTheDescriptorStillNamesTheToolsThisFacadeExpands(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(fixtures,
		"..", "descriptor", "legacy-v0", "provider_descriptor.json"))
	if err != nil {
		t.Fatalf("read the Inventory descriptor: %v", err)
	}
	var descriptor struct {
		ProvidedToolkits []struct {
			ProvidedTools []struct {
				Name       string                     `json:"name"`
				ArgsSchema map[string]json.RawMessage `json:"args_schema"`
			} `json:"provided_tools"`
			ToolkitConfig struct {
				Parameters map[string]json.RawMessage `json:"parameters"`
			} `json:"toolkit_config"`
		} `json:"provided_toolkits"`
	}
	if err := json.Unmarshal(raw, &descriptor); err != nil {
		t.Fatalf("parse the descriptor: %v", err)
	}
	naming := map[string]bool{}
	declares := map[string]bool{}
	for _, toolkit := range descriptor.ProvidedToolkits {
		for field := range toolkit.ToolkitConfig.Parameters {
			declares[field] = true
		}
		for _, tool := range toolkit.ProvidedTools {
			if _, ok := tool.ArgsSchema["toolkit_id"]; ok {
				naming[tool.Name] = true
			}
		}
	}
	for _, field := range []string{"sources", "source_configs"} {
		if !declares[field] {
			t.Errorf("the toolkit config no longer declares %q, which the gate reads", field)
		}
	}
	for _, expanding := range inventory.ExpandingTools {
		if !naming[expanding] {
			t.Errorf("%q is rewritten by this facade but the descriptor's tool "+
				"no longer takes toolkit_id", expanding)
		}
		delete(naming, expanding)
	}
	for remaining := range naming {
		t.Errorf("the descriptor's %q takes a toolkit_id but this facade does not "+
			"expand it, so it would reach the provider unexpanded", remaining)
	}
}

// ---------------------------------------------------------------------------
// the harness
// ---------------------------------------------------------------------------

// provider starts an mTLS peer that answers with a chosen status and records
// the bodies it received, and returns a facade Config wired to reach it.
//
// A REAL PEER, not a stubbed transport, for the reason DeepWiki's own tests
// give: a fake would pass while the certificate, the CA and the server name
// were all wrong, because nothing would ever complete a handshake.
func provider(t *testing.T, status int) (*[]string, facade.Config) {
	t.Helper()
	ca, caKey := authority(t, "elitea-test-ca")
	serverCert := issue(t, ca, caKey, "inventory.internal", x509.ExtKeyUsageServerAuth)
	clientCert := issue(t, ca, caKey, "elitea-main", x509.ExtKeyUsageClientAuth)
	pool := x509.NewCertPool()
	pool.AddCert(ca)

	var bodies []string
	server := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			var buffer bytes.Buffer
			_, _ = buffer.ReadFrom(r.Body)
			bodies = append(bodies, buffer.String())
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"invocation_id":"inv-1","status":"Started"}`))
		}))
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	server.StartTLS()
	t.Cleanup(server.Close)

	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")
	caFile := filepath.Join(dir, "ca.crt")
	writePEM(t, certFile, "CERTIFICATE", clientCert.Certificate[0])
	writeKey(t, keyFile, clientCert.PrivateKey.(*ecdsa.PrivateKey))
	writePEM(t, caFile, "CERTIFICATE", ca.Raw)

	return &bodies, facade.Config{
		Enabled:        true,
		BaseURL:        server.URL,
		ClientCertFile: certFile,
		ClientKeyFile:  keyFile,
		CAFile:         caFile,
		ServerName:     "inventory.internal",
		IdentitySecret: "shared-with-the-provider",
		Timeout:        10 * time.Second,
	}
}

// invoke drives one POST through the COMPOSED route: authentication,
// permissions, the tool filter, the rewrite, the hop and the revocation.
func invoke(
	t *testing.T,
	settings *countingSettings,
	minter *recordingMinter,
	tool, body string,
	providerStatus int,
) (*httptest.ResponseRecorder, *[]string) {
	t.Helper()
	bodies, cfg := provider(t, providerStatus)
	built, err := inventory.NewRoute(cfg, authConfig(),
		permissions(inventory.ReadPermission, inventory.InvokePermission),
		sources(t, settings, minter, config()),
		slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
	if err != nil {
		t.Fatalf("compose the Inventory route: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost,
		"/api/v2/inventory/tools/42/inventory/"+tool+"/invoke", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	response := httptest.NewRecorder()
	built.ServeHTTP(response, request)
	return response, bodies
}

type principalValidatorFunc func(context.Context, auth.User) (auth.User, error)

func (f principalValidatorFunc) ValidatePrincipal(
	ctx context.Context, user auth.User,
) (auth.User, error) {
	return f(ctx, user)
}

type forwardedPeerVerifierFunc func(*http.Request) error

func (f forwardedPeerVerifierFunc) VerifyForwardedIdentityPeer(request *http.Request) error {
	return f(request)
}

type permissionResolverFunc func(context.Context, auth.User, string, string) (auth.PermissionResolution, error)

func (f permissionResolverFunc) ResolvePermissions(
	ctx context.Context, user auth.User, mode, projectID string,
) (auth.PermissionResolution, error) {
	return f(ctx, user, mode, projectID)
}

func authConfig() apimw.AuthConfig {
	return apimw.AuthConfig{
		PrincipalValidator: principalValidatorFunc(
			func(_ context.Context, user auth.User) (auth.User, error) { return user, nil }),
		ForwardedIdentityVerifier: forwardedPeerVerifierFunc(
			func(*http.Request) error { return nil }),
	}
}

func permissions(granted ...string) auth.PermissionResolver {
	return permissionResolverFunc(
		func(_ context.Context, _ auth.User, _, _ string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{UserID: 11, Permissions: granted}, nil
		})
}

func authority(t *testing.T, name string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key
}

func issue(
	t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey,
	subject string, usage x509.ExtKeyUsage,
) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: subject},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     []string{subject},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	encoded := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeKey(t *testing.T, path string, key *ecdsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, path, "EC PRIVATE KEY", der)
}
