package configurations

// The DECISION half of the platform provider's model listing
// (global_provider_models.go), with no database in the way.
//
// Every case here reads what the LISTER received, or what the caller was
// refused with — never only the status code. "The listing reported models" and
// "the listing actually asked the provider, with the redeemed secret" are
// different facts, and the second is the one this route exists for.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
)

/* ── fakes ─────────────────────────────────────────────────────────────── */

// recordingLister records what it was asked to list. It implements BOTH
// interfaces, because production reads the listing capability off the composed
// connection checker — a fake that implemented only one would not stand where
// the real client stands.
type recordingLister struct {
	types   []string
	data    []map[string]any
	listing ProviderModelListing
	err     error
}

func (l *recordingLister) Check(
	context.Context, string, map[string]any,
) (ConnectionCheckResult, error) {
	return ConnectionCheckResult{}, errors.New("the model listing must not run a connection check")
}

func (l *recordingLister) ListProviderModels(
	_ context.Context, configType string, data map[string]any,
) (ProviderModelListing, error) {
	l.types = append(l.types, configType)
	l.data = append(l.data, data)
	return l.listing, l.err
}

// checkOnlyChecker is a connection checker that cannot list. It stands for a
// deployment whose gateway client predates this route.
type checkOnlyChecker struct{}

func (checkOnlyChecker) Check(
	context.Context, string, map[string]any,
) (ConnectionCheckResult, error) {
	return ConnectionCheckResult{Success: true}, nil
}

/* ── the discriminating property ───────────────────────────────────────── */

// The reason this route exists: a SAVED credential's models are read without
// the client sending the secret, because the client does not have it.
//
// The assertion is on what the LISTER received. A handler that passed the
// stored row through would hand the gateway the literal "{{secret.…}}"
// template, and the provider would refuse a credential that is perfectly good.
func TestAStoredSealedCredentialListsModelsWithTheRedeemedSecret(t *testing.T) {
	resolver := &recordingStoredResolver{resolved: map[string]any{
		"api_base": "https://api.openai.com/v1",
		"api_key":  "sk-redeemed-from-the-vault",
	}}
	lister := &recordingLister{listing: ProviderModelListing{
		Success: true, Models: []string{"gpt-4o", "gpt-4o-mini"},
	}}
	handler := NewHandler(nil,
		WithStoredConfigurationResolver(resolver),
		WithConnectionChecker(lister),
	)

	body, failure := handler.listStoredRowModels(context.Background(), "7", sealedCredentialRow())

	if failure != nil {
		t.Fatalf("listing refused: %d %q", failure.status, failure.message)
	}
	if len(lister.data) != 1 {
		t.Fatalf("the lister was called %d times, want 1. A catalogue reported without a provider "+
			"round trip is the defect this route must not reproduce.", len(lister.data))
	}
	if got := lister.data[0]["api_key"]; got != "sk-redeemed-from-the-vault" {
		t.Fatalf("the lister received api_key %v, want the redeemed value: the stored row holds a "+
			"{{secret.NAME}} reference, and passing it through asks the provider to authenticate "+
			"a template string", got)
	}
	if lister.types[0] != "open_ai" {
		t.Fatalf("the lister was called for type %q, want the STORED type", lister.types[0])
	}

	// The resolver must be asked about the row as STORED, against the project
	// in the path — never a project id read out of the row's own JSON, which
	// would redeem another project's vault.
	if len(resolver.requests) != 1 {
		t.Fatalf("the resolver was called %d times, want 1", len(resolver.requests))
	}
	request := resolver.requests[0]
	if request.ProjectID != 7 {
		t.Fatalf("resolved against project %d, want 7", request.ProjectID)
	}
	if request.AuthorID == nil || *request.AuthorID != 4964 {
		t.Fatalf("resolved with author %v, want the row's author", request.AuthorID)
	}
	if request.Data["api_key"] != "{{secret.7f3c9a2b4d5e6f708192a3b4c5d6e7f8}}" {
		t.Fatalf("the resolver was handed %v, want the STORED reference", request.Data["api_key"])
	}

	models, _ := body["models"].([]string)
	if len(models) != 2 || models[0] != "gpt-4o" {
		t.Fatalf("models = %v, want the provider's own list in its own order", body["models"])
	}
	if body["total"] != 2 {
		t.Fatalf("total = %v, want 2", body["total"])
	}
	if body["truncated"] != false {
		t.Fatalf("truncated = %v, want false", body["truncated"])
	}
	if body["type"] != "open_ai" {
		t.Fatalf("type = %v, want the row's own type", body["type"])
	}
}

/* ── the refusals ──────────────────────────────────────────────────────── */

// A row whose secret cannot be redeemed is refused, and the provider is never
// asked. Listing with an unresolved reference would report the models of
// nothing, or of whatever a literal template authenticates as.
func TestAModelListingThatCannotResolveNeverCallsTheGateway(t *testing.T) {
	resolver := &recordingStoredResolver{err: errors.New("secret 7f3c… is not in the vault")}
	lister := &recordingLister{listing: ProviderModelListing{Success: true, Models: []string{"gpt-4o"}}}
	handler := NewHandler(nil,
		WithStoredConfigurationResolver(resolver),
		WithConnectionChecker(lister),
	)

	body, failure := handler.listStoredRowModels(context.Background(), "7", sealedCredentialRow())

	if body != nil || failure == nil {
		t.Fatal("an unresolved credential reported a catalogue")
	}
	if failure.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", failure.status)
	}
	if len(lister.data) != 0 {
		t.Fatalf("the gateway was called %d times for a row that does not resolve", len(lister.data))
	}
	if strings.Contains(failure.message, "vault") {
		t.Fatalf("the message carried the internal cause: %q", failure.message)
	}
}

// A type with no lister is refused with a sentence that says the feature is
// missing — not with an empty list, which reads as "this provider offers
// nothing", and not with a failure that reads as a broken credential.
func TestAModelListingOfAnUnlistableTypeSaysSo(t *testing.T) {
	lister := &recordingLister{}
	handler := NewHandler(nil,
		WithStoredConfigurationResolver(&recordingStoredResolver{resolved: map[string]any{}}),
		WithConnectionChecker(lister),
	)
	row := sealedCredentialRow()
	row.configType = "github"

	body, failure := handler.listStoredRowModels(context.Background(), "7", row)

	if body != nil || failure == nil {
		t.Fatal("an unlistable type reported a catalogue")
	}
	if failure.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", failure.status)
	}
	if !strings.Contains(failure.message, "github") {
		t.Fatalf("message = %q, want the type named", failure.message)
	}
	if len(lister.data) != 0 {
		t.Fatalf("the gateway was called %d times for an unlistable type", len(lister.data))
	}
}

// TestTheListableTypesAreTheCheckableTypes pins the one set.
//
// The gateway serves both surfaces from the same dialects. A type this side
// would list but that side cannot answers "unsupported_type", which reads to an
// operator as a failed credential rather than a missing feature — the same
// drift check_connection.go's own header demands for its map.
//
// `open_ai_azure` and `vllm` joined the set with their gateway probes and
// listers. `anthropic` did NOT: the gateway carries neither for it.
func TestTheListableTypesAreTheCheckableTypes(t *testing.T) {
	for _, configType := range []string{
		"open_ai", "azure_open_ai", "open_ai_azure", "ai_dial", "ollama", "vllm",
		"amazon_bedrock", "vertex_ai",
	} {
		if _, ok := checkableConnectionTypes[configType]; !ok {
			t.Fatalf("type %q is listed by the gateway but not admitted here", configType)
		}
	}
	if _, ok := checkableConnectionTypes["anthropic"]; ok {
		t.Fatal("anthropic has no gateway probe or lister; admitting it produces unsupported_type")
	}
	if len(checkableConnectionTypes) != 8 {
		t.Fatalf("the checkable set has %d types; the gateway's listers cover eight. "+
			"Add the lister on that side before widening this set.", len(checkableConnectionTypes))
	}
}

// TestTheCredentialCatalogueAdvertisesOnlyCheckableTests pins the third copy of
// the same answer: the `has_test_connection` flag the catalogue publishes.
//
// The web form draws its Test connection button from that flag alone. A
// credential type that advertises the button and is not checkable answers
// "Checking connection is not supported yet", which reads as a broken feature.
// A checkable type that does not advertise it hides a control that works.
//
// The rule is scoped to ai_credentials. A toolkit type advertises the flag for
// the SDK's own check, which does not use this map.
func TestTheCredentialCatalogueAdvertisesOnlyCheckableTests(t *testing.T) {
	handler := providerHandler()
	entries := handler.catalog.PinnedEntries(GlobalProviderSection)
	if len(entries) == 0 {
		t.Fatal("the catalogue describes no credential type; this case measures nothing")
	}
	for _, entry := range entries {
		_, checkable := checkableConnectionTypes[entry.Type]
		if entry.HasTestConnection != checkable {
			t.Errorf(
				"type %q advertises has_test_connection = %v and is checkable = %v; "+
					"the button and the check must agree",
				entry.Type, entry.HasTestConnection, checkable,
			)
		}
	}
}

// A deployment with no gateway client, or with one too old to list, answers
// "not available" — never an empty catalogue and never a panic.
func TestAModelListingWithoutItsDependenciesRefusesAndDoesNotPanic(t *testing.T) {
	for name, handler := range map[string]*Handler{
		"nothing composed": NewHandler(nil),
		"no resolver": NewHandler(nil,
			WithConnectionChecker(&recordingLister{listing: ProviderModelListing{Success: true}})),
		"no lister": NewHandler(nil,
			WithStoredConfigurationResolver(&recordingStoredResolver{resolved: map[string]any{}}),
			WithConnectionChecker(checkOnlyChecker{})),
	} {
		t.Run(name, func(t *testing.T) {
			body, failure := handler.listStoredRowModels(context.Background(), "7", sealedCredentialRow())
			if body != nil || failure == nil {
				t.Fatal("an uncomposed listing reported a catalogue")
			}
			if failure.status != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", failure.status)
			}
			if failure.message != providerModelListingUnavailableMessage {
				t.Fatalf("message = %q, want the honest not-available sentence", failure.message)
			}
		})
	}
}

// A typed-nil checker boxed into the interface must not be read as a working
// lister — the trap WithProviderAdmission records, in the one place this route
// could still meet it.
func TestAModelListingSurvivesATypedNilChecker(t *testing.T) {
	var typedNil *GatewayConnectionChecker
	handler := NewHandler(nil,
		WithStoredConfigurationResolver(&recordingStoredResolver{resolved: map[string]any{}}),
		WithConnectionChecker(typedNil),
	)

	// It MUST NOT panic. The nil receiver would be dereferenced inside the
	// client, so the refusal has to come from the composition test or from the
	// method's own nil guard — either way, never a crash.
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("a typed-nil checker panicked: %v", recovered)
		}
	}()
	body, failure := handler.listStoredRowModels(context.Background(), "7", sealedCredentialRow())
	if body != nil || failure == nil {
		t.Fatal("a typed-nil checker reported a catalogue")
	}
}

// A gateway that could not be reached is a failure, never an empty catalogue.
// An empty list would be adopted as "this provider offers no models".
func TestAModelListingTransportFailureIsNotAnEmptyCatalogue(t *testing.T) {
	handler := NewHandler(nil,
		WithStoredConfigurationResolver(&recordingStoredResolver{resolved: map[string]any{"api_key": "sk-x"}}),
		WithConnectionChecker(&recordingLister{err: errors.New("dial tcp: connection refused")}),
	)

	body, failure := handler.listStoredRowModels(context.Background(), "7", sealedCredentialRow())

	if body != nil || failure == nil {
		t.Fatal("a transport failure reported a catalogue")
	}
	if failure.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", failure.status)
	}
	if strings.Contains(failure.message, "connection refused") {
		t.Fatalf("the message carried the transport error: %q", failure.message)
	}
}

// A provider verdict (a rejected key, an unreachable host) is rendered with the
// gateway's own safe sentence, and still never as an empty list.
func TestAProviderRefusalIsReportedWithItsMessage(t *testing.T) {
	handler := NewHandler(nil,
		WithStoredConfigurationResolver(&recordingStoredResolver{resolved: map[string]any{"api_key": "sk-x"}}),
		WithConnectionChecker(&recordingLister{listing: ProviderModelListing{
			Success: false, Message: "The provider rejected the credential.",
		}}),
	)

	body, failure := handler.listStoredRowModels(context.Background(), "7", sealedCredentialRow())

	if body != nil || failure == nil {
		t.Fatal("a refused credential reported a catalogue")
	}
	if failure.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", failure.status)
	}
	if failure.message != "The provider rejected the credential." {
		t.Fatalf("message = %q, want the gateway's own verdict", failure.message)
	}
}

// A project id outside the column is refused rather than narrowed: a truncated
// id would redeem another project's vault.
func TestAModelListingRefusesAProjectIDOutsideTheColumn(t *testing.T) {
	resolver := &recordingStoredResolver{resolved: map[string]any{"api_key": "sk-x"}}
	handler := NewHandler(nil,
		WithStoredConfigurationResolver(resolver),
		WithConnectionChecker(&recordingLister{listing: ProviderModelListing{Success: true}}),
	)

	body, failure := handler.listStoredRowModels(context.Background(), "4294967297", sealedCredentialRow())

	if body != nil || failure == nil {
		t.Fatal("an out-of-range project id was accepted")
	}
	if len(resolver.requests) != 0 {
		t.Fatalf("the resolver was asked %d times for an out-of-range project", len(resolver.requests))
	}
}

// A resolved payload whose endpoint points back at this platform is refused
// before the gateway is asked — the same guard the stored check applies, on the
// same RESOLVED payload, because expansion can merge a referenced row's
// api_base into it.
func TestAModelListingRefusesASelfReferentialEndpoint(t *testing.T) {
	// The origin selfref_handler_test.go's TestMain arms for this whole test
	// binary. selfLLMOrigins memoises with sync.Once, so a per-test t.Setenv
	// would be defeated by whichever test warmed the cache first.
	const selfOrigin = "https://self.elitea.test/llm/v1"
	lister := &recordingLister{listing: ProviderModelListing{Success: true, Models: []string{"gpt-4o"}}}
	handler := NewHandler(nil,
		WithStoredConfigurationResolver(&recordingStoredResolver{resolved: map[string]any{
			"api_base": selfOrigin,
			"api_key":  "sk-x",
		}}),
		WithConnectionChecker(lister),
	)

	body, failure := handler.listStoredRowModels(context.Background(), "7", sealedCredentialRow())

	if body != nil || failure == nil {
		t.Fatal("a self-referential endpoint was listed")
	}
	if len(lister.data) != 0 {
		t.Fatalf("the gateway was called %d times for a self-referential endpoint", len(lister.data))
	}
}

// An empty catalogue from a working credential is reported as an empty LIST,
// never as null: a client cannot tell a null field from a missing one, and
// both read as "this build does not answer that".
func TestAnEmptyCatalogueIsAnEmptyListNotNull(t *testing.T) {
	handler := NewHandler(nil,
		WithStoredConfigurationResolver(&recordingStoredResolver{resolved: map[string]any{"api_key": "sk-x"}}),
		WithConnectionChecker(&recordingLister{listing: ProviderModelListing{Success: true, Models: nil}}),
	)

	body, failure := handler.listStoredRowModels(context.Background(), "7", sealedCredentialRow())
	if failure != nil {
		t.Fatalf("refused: %q", failure.message)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(encoded), `"models":[]`) {
		t.Fatalf("body = %s, want an empty array", encoded)
	}
}

/* ── the gateway client ────────────────────────────────────────────────── */

// The client speaks the gateway's route, signs the hop, and returns the ids —
// and nothing else of the gateway's answer.
func TestTheGatewayClientReadsTheListing(t *testing.T) {
	var gotPath, gotBody string
	var signed bool
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		signed = r.Header.Get(llmproxy.HeaderSignature) != ""
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"reason":"ok","models":["gpt-4o","gpt-4o","  ","gpt-4o-mini"]}`))
	}))
	defer gateway.Close()

	client := NewGatewayConnectionChecker(gateway.URL, nil, "shared-secret")
	listing, err := client.ListProviderModels(
		WithConnectionCheckProjectID(context.Background(), "7"), "open_ai",
		map[string]any{"api_base": "https://api.openai.com/v1", "api_key": "sk-redeemed"})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}

	if gotPath != "/llm/v1/list_provider_models" {
		t.Fatalf("path = %q, want the gateway's listing route", gotPath)
	}
	if !signed {
		t.Fatal("the hop was not signed; the gateway refuses an unsigned request")
	}
	if !strings.Contains(gotBody, "sk-redeemed") {
		t.Fatalf("body did not carry the resolved key: %s", gotBody)
	}
	if !listing.Success {
		t.Fatalf("success = false (%q)", listing.Message)
	}
	want := []string{"gpt-4o", "gpt-4o-mini"}
	if len(listing.Models) != len(want) || listing.Models[0] != want[0] || listing.Models[1] != want[1] {
		t.Fatalf("models = %v, want %v (de-duplicated, blanks dropped, order kept)", listing.Models, want)
	}
}

// A gateway that refuses is reported as a refusal with a safe message, and the
// gateway's reason vocabulary is what produces it.
func TestTheGatewayClientRendersARefusal(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"reason":"unauthorized","detail":"the provider rejected the credential","models":[]}`))
	}))
	defer gateway.Close()

	client := NewGatewayConnectionChecker(gateway.URL, nil, "shared-secret")
	listing, err := client.ListProviderModels(context.Background(), "open_ai", map[string]any{})
	if err != nil {
		t.Fatalf("a provider verdict must not be an error: %v", err)
	}
	if listing.Success {
		t.Fatal("a refusal reported success")
	}
	if listing.Message != "The provider rejected the credential." {
		t.Fatalf("message = %q", listing.Message)
	}
	if len(listing.Models) != 0 {
		t.Fatalf("models = %v, want none", listing.Models)
	}
}

// A gateway with no such route (an older build) is an ERROR, never an empty
// catalogue: the route answers 200 for every provider verdict, so a non-200 is
// the hop failing and says nothing about the credential.
func TestTheGatewayClientTreatsAMissingRouteAsAFailure(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	}))
	defer gateway.Close()

	client := NewGatewayConnectionChecker(gateway.URL, nil, "shared-secret")
	listing, err := client.ListProviderModels(context.Background(), "open_ai", map[string]any{})
	if err == nil {
		t.Fatalf("a 404 from the gateway was reported as a catalogue: %+v", listing)
	}
	if listing.Success {
		t.Fatal("a failed hop reported success")
	}
}

// The client applies its own bounds to what the gateway sent. A bound that
// lives only at the other end of a wire is not a bound on what this process
// allocates.
func TestTheGatewayClientBoundsWhatItAccepts(t *testing.T) {
	ids := make([]string, 0, maxProviderModelIDs+10)
	for index := 0; index < maxProviderModelIDs+10; index++ {
		ids = append(ids, "model-"+strings.Repeat("x", 1)+string(rune('a'+index%26))+"-"+strconv.Itoa(index))
	}
	ids = append(ids, strings.Repeat("y", maxProviderModelIDLength+1))
	encoded, err := json.Marshal(map[string]any{"success": true, "models": ids})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	}))
	defer gateway.Close()

	client := NewGatewayConnectionChecker(gateway.URL, nil, "shared-secret")
	listing, err := client.ListProviderModels(context.Background(), "open_ai", map[string]any{})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(listing.Models) != maxProviderModelIDs {
		t.Fatalf("models = %d, want the cap %d", len(listing.Models), maxProviderModelIDs)
	}
	if !listing.Truncated {
		t.Fatal("truncated = false for a capped listing: a short list reads as the whole catalogue")
	}
	for _, id := range listing.Models {
		if len(id) > maxProviderModelIDLength {
			t.Fatalf("an oversized id was accepted: %d bytes", len(id))
		}
	}
}
