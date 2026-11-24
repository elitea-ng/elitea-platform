// shared_modelmap_test.go — the composition of issue #316 and issue #317.
//
// #316 decides WHICH rows the resolver returns: the caller's own project, then
// the public project's `shared = true` rows. #317 decides WHAT EACH ROW CARRIES:
// the advertised elitea_title and the provider's own data.name, and it maps the
// first onto the second before dispatch.
//
// The two meet in one row loop (queryScope). Neither PR's tests pin the join:
// #316 asserts model ids only, and #317 seeds one scope only. A row that comes
// from the shared scope with no provider model name therefore passes both
// suites and still sends the provider a user-authored title.
//
// Every assertion below reads what the PROVIDER received, never the HTTP
// status. A 200 hides both defects.
package llmproxy

import (
	"fmt"
	"net/http"
	"testing"
)

// ── fixture ───────────────────────────────────────────────────────────────────

const (
	// sharedTitle is advertised; sharedWire is what the provider accepts.
	sharedTitle = "Platform GPT"
	sharedWire  = "gpt-5.1"
	// unpublishedTitle/unpublishedWire live on the public project but are NOT
	// published, so neither name may reach the provider.
	unpublishedTitle = "Unpublished Model"
	unpublishedWire  = "internal-only-1"
)

// newSharedMapHandler builds a handler whose resolver reads the caller's scope
// and the public scope, with the dispatch spy in front of the provider.
func newSharedMapHandler(t *testing.T, bySchema map[string][]fakeModelRow) (http.Handler, *dispatchSpy) {
	t.Helper()
	spy := newDispatchSpy()
	resolver := NewModelResolver(ModelResolverConfig{
		DB:              &fakeModelDB{bySchema: bySchema},
		PublicProjectID: modelPublic,
	})
	return newTestHandler(spy, nil, nil, WithModelResolver(resolver)).route(), spy
}

// chatBody is a minimal valid chat request naming model.
func chatBody(model string) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
}

// ── the composition ───────────────────────────────────────────────────────────

// TestSharedModelDispatchesTheProviderWireName is the join of the two changes.
// Project 7 owns no model. The platform published one on project 1. The caller
// selects it by its advertised title, and the PROVIDER must receive the row's
// data.name.
//
// This fails if the shared scope builds a row without its provider model name,
// which is exactly what a careless conflict resolution produces.
func TestSharedModelDispatchesTheProviderWireName(t *testing.T) {
	h, spy := newSharedMapHandler(t, map[string][]fakeModelRow{
		modelCaller: {},
		modelPublic: {{title: sharedTitle, data: []byte(`{"name":"` + sharedWire + `"}`), shared: true}},
	})

	rec := postAs(t, h, "/llm/v1/chat/completions", modelCaller, chatBody(sharedTitle))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got, ok := spy.last()
	if !ok {
		t.Fatal("the provider was never called")
	}
	if got.model != sharedWire {
		t.Fatalf("the provider received model %q, want %q — a shared row must be mapped too", got.model, sharedWire)
	}
}

// TestSharedModelWithProviderPrefixSelectsTheProvider composes the shared scope
// with #317's re-split. A shared row whose data.name carries a provider prefix
// must select that provider, because the provider decides which ai_credentials
// rows the account loads for the request.
func TestSharedModelWithProviderPrefixSelectsTheProvider(t *testing.T) {
	h, spy := newSharedMapHandler(t, map[string][]fakeModelRow{
		modelCaller: {},
		modelPublic: {{
			title:  "Team Claude",
			data:   []byte(`{"name":"anthropic/claude-sonnet-4-5"}`),
			shared: true,
		}},
	})

	rec := postAs(t, h, "/llm/v1/chat/completions", modelCaller, chatBody("Team Claude"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got, ok := spy.last()
	if !ok {
		t.Fatal("the provider was never called")
	}
	if got.provider != "anthropic" || got.model != "claude-sonnet-4-5" {
		t.Fatalf("the provider received (%q, %q), want (anthropic, claude-sonnet-4-5)", got.provider, got.model)
	}
}

// TestCollidingModelIDDispatchesTheOwnRowWireName strengthens #316's collision
// rule with #317's second name.
//
// Both scopes advertise the SAME id, so the id alone cannot say which row won.
// The two rows carry DIFFERENT provider names, so the dispatched name is the
// only evidence. The caller's own row must win, which is the precedence the
// legacy _map_model_name resolver had.
func TestCollidingModelIDDispatchesTheOwnRowWireName(t *testing.T) {
	const (
		collide   = "gpt-4o"
		ownWire   = "caller-owned-wire-name"
		sharedAlt = "platform-owned-wire-name"
	)
	h, spy := newSharedMapHandler(t, map[string][]fakeModelRow{
		modelCaller: {{title: collide, data: []byte(`{"name":"` + ownWire + `"}`)}},
		modelPublic: {{title: collide, data: []byte(`{"name":"` + sharedAlt + `"}`), shared: true}},
	})

	rec := postAs(t, h, "/llm/v1/chat/completions", modelCaller, chatBody(collide))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got, ok := spy.last()
	if !ok {
		t.Fatal("the provider was never called")
	}
	if got.model != ownWire {
		t.Fatalf("the provider received model %q, want %q — the caller's own row must win the collision", got.model, ownWire)
	}
}

// ── isolation under the mapping ───────────────────────────────────────────────

// TestUnpublishedModelIsNotDispatchable is #316's isolation proof re-stated on
// the inference path. An unpublished row on the public project must not
// dispatch under EITHER of its names.
//
// The second name matters: #317 accepts a row's data.name as well as its
// advertised id. That widened the accepted name set, so the wire name of an
// unpublished row is a second way in if the shared predicate ever fails.
func TestUnpublishedModelIsNotDispatchable(t *testing.T) {
	for _, name := range []string{unpublishedTitle, unpublishedWire} {
		t.Run(name, func(t *testing.T) {
			h, spy := newSharedMapHandler(t, map[string][]fakeModelRow{
				modelCaller: {},
				modelPublic: {{
					title:  unpublishedTitle,
					data:   []byte(`{"name":"` + unpublishedWire + `"}`),
					shared: false,
				}},
			})

			rec := postAs(t, h, "/llm/v1/chat/completions", modelCaller, chatBody(name))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 for an unpublished model; body=%s", rec.Code, rec.Body.String())
			}
			if spy.count() != 0 {
				got, _ := spy.last()
				t.Fatalf("the provider was called with %+v; an unpublished row must never dispatch", got)
			}
		})
	}
}

// TestSharedListAndDispatchAgree drives the loop from the advertised list, so
// the test fails if /llm/v1/models ever advertises a shared model that the
// inference path then rejects or dispatches under the wrong name.
func TestSharedListAndDispatchAgree(t *testing.T) {
	bySchema := map[string][]fakeModelRow{
		modelCaller: {{title: "My Model", data: []byte(`{"name":"own-wire-1"}`)}},
		modelPublic: {
			{title: sharedTitle, data: []byte(`{"name":"` + sharedWire + `"}`), shared: true},
			{title: unpublishedTitle, data: []byte(`{"name":"` + unpublishedWire + `"}`), shared: false},
		},
	}
	want := map[string]string{
		"My Model":  "own-wire-1",
		sharedTitle: sharedWire,
	}

	h, spy := newSharedMapHandler(t, bySchema)
	ids := listedIDs(t, h, modelCaller)
	if len(ids) != len(want) {
		t.Fatalf("advertised ids = %v, want %d entries", ids, len(want))
	}

	for _, id := range ids {
		wire, advertised := want[id]
		if !advertised {
			t.Fatalf("/llm/v1/models advertised %q, which no scope should expose", id)
		}
		rec := postAs(t, h, "/llm/v1/chat/completions", modelCaller, chatBody(id))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body=%s", id, rec.Code, rec.Body.String())
		}
		got, ok := spy.last()
		if !ok {
			t.Fatalf("%s: the provider was never called", id)
		}
		if got.model != wire {
			t.Fatalf("%s: the provider received model %q, want %q", id, got.model, wire)
		}
	}
}
