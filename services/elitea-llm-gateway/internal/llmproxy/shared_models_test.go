package llmproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Issue #316: /llm/v1/models listed the caller's own models only, so it
// disagreed with the model picker the same user had just used — the picker
// unions the project with the public project's `shared = true` rows.
//
// These tests assert WHICH model ids come back, never just how many.

const (
	modelCaller = "7" // the calling project
	modelPublic = "1" // the platform's shared project
	modelOther  = "9" // an unrelated tenant

	sharedModelID = "platform-gpt-4o"
	ownModelID    = "my-private-model"
	otherModelID  = "tenant-9-model"
)

// newSharedResolver builds a resolver whose shared scope points at modelPublic.
func newSharedResolver(db modelRowQuerier) *ModelResolver {
	return NewModelResolver(ModelResolverConfig{DB: db, PublicProjectID: modelPublic})
}

// TestSharedModelResolvesForNonOwningProject is the core of issue #316: project
// 7 owns no model; the platform published one on project 1; project 7 must see
// exactly that model.
func TestSharedModelResolvesForNonOwningProject(t *testing.T) {
	db := &fakeModelDB{bySchema: map[string][]fakeModelRow{
		modelCaller: {},
		modelPublic: {{title: sharedModelID, shared: true}},
	}}

	got := modelIDs(newSharedResolver(db).List(context.Background(), modelCaller))
	if len(got) != 1 || got[0] != sharedModelID {
		t.Fatalf("ids = %v, want [%s]", got, sharedModelID)
	}
}

// TestForeignPrivateModelIsNotListed is the isolation half: an unpublished row
// on the public project, and an unrelated tenant's model, must both stay
// invisible — and the unrelated tenant's schema must never be queried.
func TestForeignPrivateModelIsNotListed(t *testing.T) {
	db := &fakeModelDB{bySchema: map[string][]fakeModelRow{
		modelCaller: {},
		// Present on the public project but never published.
		modelPublic: {{title: "unpublished-model", shared: false}},
		modelOther:  {{title: otherModelID, shared: true}},
	}}

	got := modelIDs(newSharedResolver(db).List(context.Background(), modelCaller))
	if len(got) != 0 {
		t.Fatalf("ids = %v, want none (nothing was published to this project)", got)
	}
	for _, q := range db.gotSQL {
		if strings.Contains(q, `"p_`+modelOther+`"`) {
			t.Fatalf("resolver queried an unrelated tenant's schema: %s", q)
		}
	}
	models := db.modelStatements()
	if len(models) != 2 {
		t.Fatalf("got %d model queries, want 2 (own + public)", len(models))
	}
	if strings.Contains(models[0], "shared = true") {
		t.Error("the caller's OWN scope must not be filtered to shared rows")
	}
	if !strings.Contains(models[1], "shared = true") {
		t.Error("the public scope MUST carry the shared predicate")
	}
	// Issue #451 reads the caller scope and the public shared scope. Published
	// models also get a distinct owner-metadata lookup without the predicate;
	// only those model rows may turn its result into a scoped capability.
	creds := db.credentialStatements()
	if len(creds) != 3 {
		t.Fatalf("got %d credential queries, want 3 (own + public shared + model owner)", len(creds))
	}
	if strings.Contains(creds[0], "shared = true") {
		t.Error("the caller's OWN credential scope must not be filtered to shared rows")
	}
	if !strings.Contains(creds[1], "shared = true") {
		t.Error("the public credential scope MUST carry the shared predicate")
	}
	if strings.Contains(creds[2], "shared = true") {
		t.Error("the published model's owner lookup must retain unshared owner credentials")
	}
}

// TestOwnAndSharedModelsUnion: the caller sees its own models AND the published
// ones, own first.
func TestOwnAndSharedModelsUnion(t *testing.T) {
	db := &fakeModelDB{bySchema: map[string][]fakeModelRow{
		modelCaller: {{title: ownModelID}},
		modelPublic: {{title: sharedModelID, shared: true}},
	}}

	got := modelIDs(newSharedResolver(db).List(context.Background(), modelCaller))
	want := []string{ownModelID, sharedModelID}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ids = %v, want %v (own project first)", got, want)
	}
}

// TestCollidingModelIDAppearsOnce pins the ordering/collision rule the issue
// asks to decide. When a project and the public project expose the SAME model
// id, the caller sees ONE entry, and it is the project's own row — the
// precedence the legacy _map_model_name resolver had, which probed
// {project}_{model} before {public}_{model}.
//
// The two rows are made distinguishable through the id-resolution path: the own
// row carries the id as its elitea_title alias, the public row carries it only
// as data.name. A duplicate would therefore show up as a second entry.
func TestCollidingModelIDAppearsOnce(t *testing.T) {
	const collide = "gpt-4o"
	db := &fakeModelDB{bySchema: map[string][]fakeModelRow{
		modelCaller: {{title: collide}},
		modelPublic: {{title: "", data: []byte(`{"name":"` + collide + `"}`), shared: true}},
	}}

	got := modelIDs(newSharedResolver(db).List(context.Background(), modelCaller))
	if len(got) != 1 || got[0] != collide {
		t.Fatalf("ids = %v, want exactly [%s] — the own row wins and is not duplicated", got, collide)
	}
}

// TestSharedModelScopeOffByDefault: with no public project configured the
// resolver behaves exactly as before issue #316.
func TestSharedModelScopeOffByDefault(t *testing.T) {
	db := &fakeModelDB{bySchema: map[string][]fakeModelRow{
		modelCaller: {{title: ownModelID}},
		modelPublic: {{title: sharedModelID, shared: true}},
	}}
	r := NewModelResolver(ModelResolverConfig{DB: db}) // no PublicProjectID

	got := modelIDs(r.List(context.Background(), modelCaller))
	if len(got) != 1 || got[0] != ownModelID {
		t.Fatalf("ids = %v, want [%s] only", got, ownModelID)
	}
	if len(db.gotSQL) != queriesPerScope {
		t.Fatalf("got %d queries, want %d (own scope only)", len(db.gotSQL), queriesPerScope)
	}
}

// TestPublicProjectCallerListsModelsOnce: when the caller IS the public project
// the second read is skipped, so a shared model is not listed twice.
func TestPublicProjectCallerListsModelsOnce(t *testing.T) {
	db := &fakeModelDB{bySchema: map[string][]fakeModelRow{
		modelPublic: {{title: sharedModelID, shared: true}},
	}}

	got := modelIDs(newSharedResolver(db).List(context.Background(), modelPublic))
	if len(got) != 1 || got[0] != sharedModelID {
		t.Fatalf("ids = %v, want [%s] exactly once", got, sharedModelID)
	}
	if len(db.gotSQL) != queriesPerScope {
		t.Fatalf("got %d queries, want %d", len(db.gotSQL), queriesPerScope)
	}
}

// TestSharedModelScopeBackstopRejectsUnpublishedRow proves the Go-side check is
// a real backstop: with the SQL predicate defeated, an unpublished model must
// still not be advertised. The resolver has no cache here, so the failed read
// yields an empty set rather than another project's model.
func TestSharedModelScopeBackstopRejectsUnpublishedRow(t *testing.T) {
	db := &fakeModelDB{
		ignoreSharedPredicate: true,
		bySchema: map[string][]fakeModelRow{
			modelCaller: {},
			modelPublic: {{title: "unpublished-model", shared: false}},
		},
	}

	got := modelIDs(newSharedResolver(db).List(context.Background(), modelCaller))
	if len(got) != 0 {
		t.Fatalf("ids = %v, want none — an unpublished row escaped the shared scope", got)
	}
}

// TestSharedModelAppearsOnModelsEndpoint is the behavioural assertion issue #316
// asks for: a project that owns no model still sees the platform's shared model
// on GET /llm/v1/models, so the endpoint agrees with the picker.
func TestSharedModelAppearsOnModelsEndpoint(t *testing.T) {
	db := &fakeModelDB{bySchema: map[string][]fakeModelRow{
		modelCaller: {},
		modelPublic: {{title: sharedModelID, shared: true}},
	}}
	resolver := newSharedResolver(db)
	h := newTestHandler(&fakeRouter{}, nil, nil, WithModelResolver(resolver)).route()

	rec := getModels(t, h, "/llm/v1/models", modelCaller)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var list modelsList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(list.Data) != 1 || list.Data[0].ID != sharedModelID {
		t.Fatalf("models = %v, want [%s]", modelIDs(list.Data), sharedModelID)
	}

	// The same model must also resolve as a single-model lookup, so a caller can
	// address it directly.
	if _, ok := resolver.Get(context.Background(), modelCaller, sharedModelID); !ok {
		t.Errorf("Get(%s) not found for the non-owning project", sharedModelID)
	}
}
