// model_credential_link_test.go — issue #451, the provider half.
//
// A model configuration row names ONE credential. That credential's type is the
// only statement of which provider serves the model. Nothing in the gateway
// read it, so the provider came from a prefix inside the model name alone.
//
// EVERY row in the staging database of 2026-07-09 holds a BARE name: 48 of 48
// chat rows and 8 of 8 embedding rows, in 11 and 4 project schemas. No name and
// no title in that database contains a slash. So the prefix path names a
// provider for none of them, and bifrost/core refuses an empty provider
// (core@v1.7.3 utils.go:152-155). Every one of those models is undispatchable.
//
// The rows here are shaped like those rows: a bare data.name, and a
// data.ai_credentials object of the shape the platform actually persists —
// {"elitea_title": ..., "private": ...}. The tests assert the provider the
// ROUTER received. A 200 is not evidence: before this change the same calls
// returned 200 from a fake router while a real bifrost/core would have refused
// them.
package llmproxy

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/account"
)

// ── harness ───────────────────────────────────────────────────────────────────

// linkSpy adds one observation to dispatchSpy: the credential the handler
// pinned on the request context. The pin is what the account package reads, so
// a test that does not look at it cannot tell a resolved link from an ignored
// one.
type linkSpy struct {
	*dispatchSpy

	mu     sync.Mutex
	link   account.LinkedCredential
	pinned bool
}

func (s *linkSpy) ChatCompletionRequest(
	ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest,
) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	s.capture(ctx)
	return s.dispatchSpy.ChatCompletionRequest(ctx, req)
}

func (s *linkSpy) capture(ctx *schemas.BifrostContext) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.link, s.pinned = ctx.Value(account.ContextKeyLinkedCredential).(account.LinkedCredential)
}

func (s *linkSpy) pin() (account.LinkedCredential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.link, s.pinned
}

var _ LLMRouter = (*linkSpy)(nil)

// newLinkHandler builds a handler over model rows and credential rows for the
// caller's project, and returns it with the spy that observes the router.
func newLinkHandler(t *testing.T, rows []fakeModelRow, creds []fakeCredentialRow) (http.Handler, *linkSpy) {
	t.Helper()
	spy := &linkSpy{dispatchSpy: newDispatchSpy()}
	db := &fakeModelDB{
		rows:          rows,
		credsBySchema: map[string][]fakeCredentialRow{mapProjectID: creds},
	}
	h := NewHandler(spy, nil, nil, WithModelResolver(NewModelResolver(ModelResolverConfig{DB: db})))
	return h.route(), spy
}

// stagingShapedRow is a model row shaped like a real one: a user-authored
// title, a BARE wire name, and a credential link that names the credential by
// title. All 58 model rows of the staging dump have exactly this shape.
func stagingShapedRow(title, wireName, credentialTitle string) fakeModelRow {
	return fakeModelRow{
		title: title,
		data: []byte(fmt.Sprintf(
			`{"name":%q,"ai_credentials":{"elitea_title":%q,"private":false}}`,
			wireName, credentialTitle)),
	}
}

// ── the headline proof ────────────────────────────────────────────────────────

// TestLinkedCredentialGivesTheProviderForABareName is the fix, on every dialect
// that dispatches a model.
//
// Delete the link-reading in mapModel and this test fails on every dialect: the
// provider is then the empty string, which is the value bifrost/core rejects.
func TestLinkedCredentialGivesTheProviderForABareName(t *testing.T) {
	for _, d := range mappedDialects() {
		t.Run(d.name, func(t *testing.T) {
			h, spy := newLinkHandler(t,
				[]fakeModelRow{stagingShapedRow("Prod GPT", "gpt-4o", "team-azure")},
				[]fakeCredentialRow{{id: "cred-uuid-1", typ: "azure_open_ai", title: "team-azure"}})

			rec := postAs(t, h, d.path, mapProjectID, d.body("Prod GPT"))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			got, ok := spy.last()
			if !ok {
				t.Fatal("the router was never called")
			}
			if got.provider != "azure" {
				t.Errorf("router received provider %q, want %q; "+
					"an empty provider is what bifrost/core refuses", got.provider, "azure")
			}
			if got.model != "gpt-4o" {
				t.Errorf("router received model %q, want %q", got.model, "gpt-4o")
			}
		})
	}
}

// TestBareNameWithoutALinkHasNoProvider is the control. It measures the defect
// itself: the SAME bare name, with no credential link to read, reaches the
// router with an empty provider. That is the state every staging row is in
// today, and it is the state the test above proves the link repairs.
//
// It also pins the deliberate limit of this change: a row with no link is left
// exactly as it was. The standalone seed writes such rows.
func TestBareNameWithoutALinkHasNoProvider(t *testing.T) {
	h, spy := newLinkHandler(t,
		[]fakeModelRow{{title: "Prod GPT", data: []byte(`{"name":"gpt-4o"}`)}},
		nil)

	postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"Prod GPT","messages":[{"role":"user","content":"hi"}]}`)

	got, ok := spy.last()
	if !ok {
		t.Fatal("the router was never called")
	}
	if got.provider != "" {
		t.Fatalf("router received provider %q, want \"\"; "+
			"a bare name with no link has no provider to read", got.provider)
	}
}

// ── every credential type ─────────────────────────────────────────────────────

// TestEveryCredentialTypeGivesItsProvider walks every configuration type the
// gateway serves and asserts the provider name the router received.
//
// The six rows of the issue's table are here, and so are the two types the
// gateway added since (anthropic, vllm). The provider is asserted as the string
// the router saw, not as the constant the code used, so a rename on either side
// is visible.
func TestEveryCredentialTypeGivesItsProvider(t *testing.T) {
	cases := []struct {
		credentialType string
		wantProvider   string
	}{
		{"open_ai", "openai"},
		{"azure_open_ai", "azure"},
		{"open_ai_azure", "azure"},
		{"ai_dial", "azure"},
		{"amazon_bedrock", "bedrock"},
		{"vertex_ai", "vertex"},
		{"ollama", "ollama"},
		{"anthropic", "anthropic"},
		{"vllm", "vllm"},
	}
	for _, tc := range cases {
		t.Run(tc.credentialType, func(t *testing.T) {
			h, spy := newLinkHandler(t,
				[]fakeModelRow{stagingShapedRow("The Model", "some-bare-model", "the-credential")},
				[]fakeCredentialRow{{id: "c1", typ: tc.credentialType, title: "the-credential"}})

			postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
				`{"model":"The Model","messages":[{"role":"user","content":"hi"}]}`)

			got, ok := spy.last()
			if !ok {
				t.Fatal("the router was never called")
			}
			if got.provider != tc.wantProvider {
				t.Errorf("credential type %q gave provider %q, want %q",
					tc.credentialType, got.provider, tc.wantProvider)
			}
			if got.model != "some-bare-model" {
				t.Errorf("router received model %q, want %q", got.model, "some-bare-model")
			}
		})
	}
}

// TestOpenAICompatibleCredentialBaseUsesVLLMProvider proves that the existing
// open_ai credential can keep a tenant-specific api_base after the Bifrost
// migration. The model flag selects the worker dialect; the credential base
// selects the gateway provider that can carry that endpoint per key.
func TestOpenAICompatibleCredentialBaseUsesVLLMProvider(t *testing.T) {
	h, spy := newLinkHandler(t,
		[]fakeModelRow{stagingShapedRow(
			"Sonnet compatible", "eu.anthropic.claude-sonnet-4-6", "ai-creds",
		)},
		[]fakeCredentialRow{{
			id: "c1", typ: "open_ai", title: "ai-creds",
			apiBase: "https://proxy.example/llm/v1",
		}})

	postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"Sonnet compatible","messages":[{"role":"user","content":"hi"}]}`)

	got, ok := spy.last()
	if !ok {
		t.Fatal("the router was never called")
	}
	if got.provider != "vllm" {
		t.Fatalf("provider = %q, want %q", got.provider, "vllm")
	}
	if got.model != "eu.anthropic.claude-sonnet-4-6" {
		t.Fatalf("model = %q, want Sonnet wire name", got.model)
	}
}

func TestOfficialOpenAICredentialBaseKeepsOpenAIProvider(t *testing.T) {
	h, spy := newLinkHandler(t,
		[]fakeModelRow{stagingShapedRow("GPT", "gpt-5.4-mini", "ai-creds")},
		[]fakeCredentialRow{{
			id: "c1", typ: "open_ai", title: "ai-creds",
			apiBase: "https://api.openai.com/v1",
		}})

	postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"GPT","messages":[{"role":"user","content":"hi"}]}`)

	got, ok := spy.last()
	if !ok {
		t.Fatal("the router was never called")
	}
	if got.provider != "openai" {
		t.Fatalf("provider = %q, want %q", got.provider, "openai")
	}
}

// TestCredentialTypesOfTheStagingDumpAreAllServed names the four types the
// staging dump actually holds on a model link, so a change that dropped one of
// them from the table would fail here rather than in production.
func TestCredentialTypesOfTheStagingDumpAreAllServed(t *testing.T) {
	// Counted on the staging dump of 2026-07-09, over the 51 model rows whose
	// link resolves inside its own schema.
	for _, typ := range []string{"ai_dial", "azure_open_ai", "amazon_bedrock", "open_ai"} {
		if _, ok := account.ProviderForCredentialType(typ); !ok {
			t.Errorf("credential type %q serves model rows in staging but maps to no provider", typ)
		}
	}
}

// ── the pin ───────────────────────────────────────────────────────────────────

// TestResolvedLinkPinsTheCredentialOnTheContext proves the second half of the
// fix is wired: the account package is told WHICH credential to use.
func TestResolvedLinkPinsTheCredentialOnTheContext(t *testing.T) {
	h, spy := newLinkHandler(t,
		[]fakeModelRow{stagingShapedRow("Prod GPT", "gpt-4o", "team-azure")},
		[]fakeCredentialRow{{id: "cred-uuid-1", typ: "azure_open_ai", title: "team-azure"}})

	postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"Prod GPT","messages":[{"role":"user","content":"hi"}]}`)

	link, pinned := spy.pin()
	if !pinned {
		t.Fatal("no credential was pinned on the request context")
	}
	want := account.LinkedCredential{ProjectID: mapProjectID, ConfigID: "cred-uuid-1", Title: "team-azure"}
	if link != want {
		t.Fatalf("pinned credential = %+v, want %+v", link, want)
	}
}

// TestUnlinkedModelPinsNothing is the control for the pin. With nothing pinned
// the account package keeps offering every credential of the provider, which is
// the pre-#451 behaviour a seeded row still depends on.
func TestUnlinkedModelPinsNothing(t *testing.T) {
	h, spy := newLinkHandler(t,
		[]fakeModelRow{{title: "vllm/E2E-MOCK-MODEL", data: []byte(`{"name":"vllm/E2E-MOCK-MODEL"}`)}},
		nil)

	postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"vllm/E2E-MOCK-MODEL","messages":[{"role":"user","content":"hi"}]}`)

	if _, pinned := spy.pin(); pinned {
		t.Fatal("a model with no credential link pinned a credential")
	}
}

// ── the prefix path is not disturbed ──────────────────────────────────────────

// TestSeedShapedRowKeepsThePrefixBehaviour is acceptance criterion 3. The
// standalone seed writes a `vllm/` prefix on purpose and links no credential.
// That row must dispatch exactly as it did before.
func TestSeedShapedRowKeepsThePrefixBehaviour(t *testing.T) {
	h, spy := newLinkHandler(t,
		[]fakeModelRow{{
			title: "vllm/E2E-MOCK-MODEL",
			data:  []byte(`{"name":"vllm/E2E-MOCK-MODEL"}`),
		}},
		nil)

	postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"vllm/E2E-MOCK-MODEL","messages":[{"role":"user","content":"hi"}]}`)

	got, _ := spy.last()
	if got.provider != "vllm" || got.model != "E2E-MOCK-MODEL" {
		t.Fatalf("seed-shaped row dispatched (%q, %q), want (%q, %q)",
			got.provider, got.model, "vllm", "E2E-MOCK-MODEL")
	}
}

// TestDanglingLinkKeepsThePrefixBehaviour covers a link that names a credential
// the caller's scopes do not hold. The staging dump holds 5 such rows of 56.
//
// The gateway must NOT refuse the request here. A prefixed name still
// dispatches today through the prefix path, and refusing would take that away.
// The row is therefore left exactly as it was, and the miss is logged.
func TestDanglingLinkKeepsThePrefixBehaviour(t *testing.T) {
	h, spy := newLinkHandler(t,
		[]fakeModelRow{stagingShapedRow("Team Claude", "anthropic/claude-sonnet-4-5", "deleted-credential")},
		[]fakeCredentialRow{{id: "c1", typ: "open_ai", title: "a-different-credential"}})

	rec := postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"Team Claude","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; a dangling link must not take away the prefix path", rec.Code)
	}
	got, _ := spy.last()
	if got.provider != "anthropic" || got.model != "claude-sonnet-4-5" {
		t.Fatalf("dangling link dispatched (%q, %q), want (%q, %q)",
			got.provider, got.model, "anthropic", "claude-sonnet-4-5")
	}
	// Nothing may be pinned: the credential that would be pinned does not exist,
	// and pinning it would refuse a request the prefix path can still serve.
	if _, pinned := spy.pin(); pinned {
		t.Fatal("a dangling link pinned a credential")
	}
}

// TestUnservedCredentialTypeKeepsThePrefixBehaviour covers a credential type the
// gateway has no provider for. It must not become an empty provider, and it
// must not become a prefix either: a prefix bifrost does not know is discarded
// by ParseModelString, which would fail later and less clearly.
func TestUnservedCredentialTypeKeepsThePrefixBehaviour(t *testing.T) {
	h, spy := newLinkHandler(t,
		[]fakeModelRow{stagingShapedRow("Odd One", "openai/gpt-4o", "the-credential")},
		[]fakeCredentialRow{{id: "c1", typ: "some_future_provider", title: "the-credential"}})

	postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"Odd One","messages":[{"role":"user","content":"hi"}]}`)

	got, _ := spy.last()
	if got.provider != "openai" || got.model != "gpt-4o" {
		t.Fatalf("unserved type dispatched (%q, %q), want (%q, %q)",
			got.provider, got.model, "openai", "gpt-4o")
	}
	if _, pinned := spy.pin(); pinned {
		t.Fatal("an unserved credential type pinned a credential")
	}
}

// TestLinkOverridesThePrefix pins the precedence. The link is an explicit,
// structured statement of which provider serves the model; a prefix is a
// substring of a name. The deleted LiteLLM mapper made the same choice.
func TestLinkOverridesThePrefix(t *testing.T) {
	h, spy := newLinkHandler(t,
		[]fakeModelRow{stagingShapedRow("Mislabelled", "openai/gpt-4o", "really-azure")},
		[]fakeCredentialRow{{id: "c1", typ: "azure_open_ai", title: "really-azure"}})

	postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"Mislabelled","messages":[{"role":"user","content":"hi"}]}`)

	got, _ := spy.last()
	if got.provider != "azure" {
		t.Fatalf("provider = %q, want %q (the link wins over the name prefix)", got.provider, "azure")
	}
	if got.model != "gpt-4o" {
		t.Fatalf("model = %q, want %q (the prefix is still stripped from the wire name)", got.model, "gpt-4o")
	}
}

// ── the second link shape ─────────────────────────────────────────────────────

// TestExpandedLinkShapeGivesTheProvider covers the shape that carries the
// credential type in the model row itself. The legacy platform built it in
// memory and the deleted mapper read it; elitea-main writes the same three keys
// when it freezes a configuration reference. No staging row holds it, but a row
// CAN, and reading only one of the two shapes would leave a silent gap.
func TestExpandedLinkShapeGivesTheProvider(t *testing.T) {
	h, spy := newLinkHandler(t,
		[]fakeModelRow{{
			title: "Prod GPT",
			data: []byte(`{"name":"gpt-4o","ai_credentials":{` +
				`"configuration_type":"azure_open_ai",` +
				`"configuration_uuid":"11111111-2222-3333-4444-555555555555",` +
				`"configuration_project_id":42}}`),
		}},
		nil) // no credential read is needed: the row answers by itself

	postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"Prod GPT","messages":[{"role":"user","content":"hi"}]}`)

	got, _ := spy.last()
	if got.provider != "azure" || got.model != "gpt-4o" {
		t.Fatalf("expanded shape dispatched (%q, %q), want (%q, %q)",
			got.provider, got.model, "azure", "gpt-4o")
	}
	link, pinned := spy.pin()
	if !pinned {
		t.Fatal("the expanded shape pinned no credential")
	}
	want := account.LinkedCredential{
		ProjectID: "42",
		ConfigID:  "11111111-2222-3333-4444-555555555555",
	}
	if link != want {
		t.Fatalf("pinned credential = %+v, want %+v", link, want)
	}
}

// TestPreDebrandingLinkSpellingIsRead covers `alita_title`, the spelling every
// row carried before the debranding. A database that has not run the rename
// task still holds it, and reading only the new spelling would leave those rows
// exactly as broken as before.
func TestPreDebrandingLinkSpellingIsRead(t *testing.T) {
	h, spy := newLinkHandler(t,
		[]fakeModelRow{{
			title: "Prod GPT",
			data:  []byte(`{"name":"gpt-4o","ai_credentials":{"alita_title":"team-azure","private":false}}`),
		}},
		[]fakeCredentialRow{{id: "c1", typ: "azure_open_ai", title: "team-azure"}})

	postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"Prod GPT","messages":[{"role":"user","content":"hi"}]}`)

	if got, _ := spy.last(); got.provider != "azure" {
		t.Fatalf("provider = %q, want %q", got.provider, "azure")
	}
}

// ── scopes ────────────────────────────────────────────────────────────────────

// TestModelLinksToAPublishedCredential covers a model row of the caller's own
// project that names a credential the PLATFORM published. The staging dump
// holds one such row. The two scopes read here are the same two scopes the
// account package reads, so the model resolver and the credential resolver
// agree on what is reachable.
func TestModelLinksToAPublishedCredential(t *testing.T) {
	const public = "1"
	spy := &linkSpy{dispatchSpy: newDispatchSpy()}
	db := &fakeModelDB{
		bySchema: map[string][]fakeModelRow{
			mapProjectID: {stagingShapedRow("Prod GPT", "gpt-4o", "platform-dial")},
			public:       {},
		},
		credsBySchema: map[string][]fakeCredentialRow{
			mapProjectID: {},
			public:       {{id: "pub-1", typ: "ai_dial", title: "platform-dial", shared: true}},
		},
	}
	resolver := NewModelResolver(ModelResolverConfig{DB: db, PublicProjectID: public})
	h := NewHandler(spy, nil, nil, WithModelResolver(resolver)).route()

	postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"Prod GPT","messages":[{"role":"user","content":"hi"}]}`)

	if got, _ := spy.last(); got.provider != "azure" {
		t.Fatalf("provider = %q, want %q (ai_dial is served by azure)", got.provider, "azure")
	}
	link, pinned := spy.pin()
	if !pinned {
		t.Fatal("no credential was pinned")
	}
	// The pin must name the PUBLIC project, because that is the schema whose
	// vault holds the secret. Naming the caller would resolve nothing.
	if link.ProjectID != public || link.ConfigID != "pub-1" {
		t.Fatalf("pinned credential = %+v, want project %q id %q", link, public, "pub-1")
	}
}

// TestUnpublishedCredentialOfThePublicProjectIsNotLinkable is the isolation
// half. A credential the platform did NOT publish must not resolve a link, even
// though it sits in the schema the gateway reads for shared rows.
func TestUnpublishedCredentialOfThePublicProjectIsNotLinkable(t *testing.T) {
	const public = "1"
	spy := &linkSpy{dispatchSpy: newDispatchSpy()}
	db := &fakeModelDB{
		bySchema: map[string][]fakeModelRow{
			mapProjectID: {stagingShapedRow("Prod GPT", "gpt-4o", "not-published")},
			public:       {},
		},
		credsBySchema: map[string][]fakeCredentialRow{
			mapProjectID: {},
			public:       {{id: "pub-1", typ: "ai_dial", title: "not-published", shared: false}},
		},
	}
	resolver := NewModelResolver(ModelResolverConfig{DB: db, PublicProjectID: public})
	h := NewHandler(spy, nil, nil, WithModelResolver(resolver)).route()

	postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"Prod GPT","messages":[{"role":"user","content":"hi"}]}`)

	if _, pinned := spy.pin(); pinned {
		t.Fatal("an unpublished credential of the public project resolved a link")
	}
	if got, _ := spy.last(); got.provider != "" {
		t.Fatalf("provider = %q, want \"\" (nothing legitimate could name one)", got.provider)
	}
}

// TestPublishedModelPinsItsUnpublishedOwnerCredential is the restored Centry
// shape: the model row is shared, its private:false link names a credential in
// the same public project, and that credential is intentionally not shared.
// The model is the capability; the credential must not become generally
// linkable as a side effect.
func TestPublishedModelPinsItsUnpublishedOwnerCredential(t *testing.T) {
	const public = "1"
	spy := &linkSpy{dispatchSpy: newDispatchSpy()}
	db := &fakeModelDB{
		bySchema: map[string][]fakeModelRow{
			mapProjectID: {},
			public: {{
				title:  "Shared GPT",
				data:   []byte(`{"name":"gpt-5.4-mini","ai_credentials":{"elitea_title":"ai_creds","private":false}}`),
				shared: true,
			}},
		},
		credsBySchema: map[string][]fakeCredentialRow{
			mapProjectID: {},
			public:       {{id: "pub-1", typ: "open_ai", title: "ai_creds", shared: false}},
		},
	}
	resolver := NewModelResolver(ModelResolverConfig{DB: db, PublicProjectID: public})
	h := NewHandler(spy, nil, nil, WithModelResolver(resolver)).route()

	postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"Shared GPT","messages":[{"role":"user","content":"hi"}]}`)

	if got, _ := spy.last(); got.provider != "openai" || got.model != "gpt-5.4-mini" {
		t.Fatalf("dispatch = (%q, %q), want (openai, gpt-5.4-mini)", got.provider, got.model)
	}
	link, pinned := spy.pin()
	if !pinned {
		t.Fatal("published model did not pin its owner credential")
	}
	if link.ProjectID != public || link.ConfigID != "pub-1" || !link.ModelOwnerAccess {
		t.Fatalf("pinned credential = %+v, want public owner capability", link)
	}
}

// TestPublishedModelPrivateLinkUsesTheCallerScope preserves the other current
// reference mode. private:true never grants model-owner access and may resolve
// only the caller's personal credential (or a separately published fallback).
func TestPublishedModelPrivateLinkUsesTheCallerScope(t *testing.T) {
	const public = "1"
	spy := &linkSpy{dispatchSpy: newDispatchSpy()}
	db := &fakeModelDB{
		bySchema: map[string][]fakeModelRow{
			mapProjectID: {},
			public: {{
				title:  "Personal GPT",
				data:   []byte(`{"name":"gpt-5.4-mini","ai_credentials":{"elitea_title":"personal_creds","private":true}}`),
				shared: true,
			}},
		},
		credsBySchema: map[string][]fakeCredentialRow{
			mapProjectID: {{id: "own-1", typ: "open_ai", title: "personal_creds"}},
			public:       {{id: "pub-1", typ: "amazon_bedrock", title: "personal_creds", shared: false}},
		},
	}
	resolver := NewModelResolver(ModelResolverConfig{DB: db, PublicProjectID: public})
	h := NewHandler(spy, nil, nil, WithModelResolver(resolver)).route()

	postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"Personal GPT","messages":[{"role":"user","content":"hi"}]}`)

	link, pinned := spy.pin()
	if !pinned || link.ProjectID != mapProjectID || link.ConfigID != "own-1" {
		t.Fatalf("pinned credential = %+v, want caller's personal credential", link)
	}
	if link.ModelOwnerAccess {
		t.Fatal("private:true link received model-owner access")
	}
}

// TestOwnCredentialWinsOverAPublishedOneOfTheSameTitle pins the precedence
// between the two scopes. It is the same precedence the model list and the
// credential list already keep.
func TestOwnCredentialWinsOverAPublishedOneOfTheSameTitle(t *testing.T) {
	const public = "1"
	spy := &linkSpy{dispatchSpy: newDispatchSpy()}
	db := &fakeModelDB{
		bySchema: map[string][]fakeModelRow{
			mapProjectID: {stagingShapedRow("Prod GPT", "gpt-4o", "shared-name")},
			public:       {},
		},
		credsBySchema: map[string][]fakeCredentialRow{
			mapProjectID: {{id: "own-1", typ: "open_ai", title: "shared-name"}},
			public:       {{id: "pub-1", typ: "amazon_bedrock", title: "shared-name", shared: true}},
		},
	}
	resolver := NewModelResolver(ModelResolverConfig{DB: db, PublicProjectID: public})
	h := NewHandler(spy, nil, nil, WithModelResolver(resolver)).route()

	postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"Prod GPT","messages":[{"role":"user","content":"hi"}]}`)

	if got, _ := spy.last(); got.provider != "openai" {
		t.Fatalf("provider = %q, want %q (the caller's own credential wins)", got.provider, "openai")
	}
	link, _ := spy.pin()
	if link.ProjectID != mapProjectID || link.ConfigID != "own-1" {
		t.Fatalf("pinned credential = %+v, want the caller's own row", link)
	}
}

// ── the credential read itself ────────────────────────────────────────────────

// TestCredentialReadSelectsNoSecret proves the model resolver's new statement
// cannot carry secret material. It reads three identifier columns and the
// non-secret api_base only; the key stays in the account package, which
// resolves it per request through the Fernet vault.
func TestCredentialReadSelectsNoSecret(t *testing.T) {
	db := &fakeModelDB{rows: []fakeModelRow{{title: "gpt-4o"}}}
	NewModelResolver(ModelResolverConfig{DB: db}).List(t.Context(), mapProjectID)

	creds := db.credentialStatements()
	if len(creds) != 1 {
		t.Fatalf("got %d credential statements, want 1", len(creds))
	}
	if strings.Count(creds[0], "c.data") != 1 ||
		!strings.Contains(creds[0], "c.data->>'api_base'") {
		t.Fatalf("the credential statement reads more than the allowed api_base:\n%s", creds[0])
	}
	for _, forbidden := range []string{"api_key", "api_token"} {
		if strings.Contains(creds[0], forbidden) {
			t.Fatalf("the credential statement names %q:\n%s", forbidden, creds[0])
		}
	}
	if !strings.Contains(creds[0], "status_ok") {
		t.Fatalf("the credential statement does not filter on status_ok:\n%s", creds[0])
	}
}
