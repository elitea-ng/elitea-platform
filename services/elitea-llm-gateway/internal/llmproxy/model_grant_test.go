package llmproxy

// The gateway half of the platform-model grant.
//
// The rule is stated in model_grant.go: a catalogue row is offered to every
// project (`all`, and the absent value), to none, or to the ids in
// `shared_with`. elitea-main applies it to the model LIST; these tests are the
// evidence that a project which cannot SEE a model also cannot dispatch it,
// which is the half a list-only rule leaves open — a model id is a string, and
// a project that once held a grant would go on sending it.
//
// Every case asserts WHICH ids come back, never how many.

import (
	"context"
	"testing"
)

// grantedRow builds a shared catalogue row with an explicit grant.
func grantedRow(id, scope string, sharedWith string) fakeModelRow {
	data := `{"name":"` + id + `","share_scope":"` + scope + `"`
	if sharedWith != "" {
		data += `,"shared_with":` + sharedWith
	}
	data += `}`
	return fakeModelRow{title: id, data: []byte(data), shared: true}
}

// TestCatalogueModelGrantedToAllIsDispatchable is the compatible case, and it
// is asserted first because it is the one every existing deployment is in.
func TestCatalogueModelGrantedToAllIsDispatchable(t *testing.T) {
	db := &fakeModelDB{bySchema: map[string][]fakeModelRow{
		modelCaller: {},
		modelPublic: {grantedRow("granted-to-all", "all", "")},
	}}

	got := modelIDs(newSharedResolver(db).List(context.Background(), modelCaller))
	if len(got) != 1 || got[0] != "granted-to-all" {
		t.Fatalf("ids = %v, want [granted-to-all]", got)
	}
}

// TestCatalogueModelGrantedToNoProjectIsNotDispatchable: `none` withdraws the
// model from every project while leaving the row in the catalogue, so the
// operator who set it can set it back.
func TestCatalogueModelGrantedToNoProjectIsNotDispatchable(t *testing.T) {
	db := &fakeModelDB{bySchema: map[string][]fakeModelRow{
		modelCaller: {},
		modelPublic: {grantedRow("withdrawn", "none", "")},
	}}

	got := modelIDs(newSharedResolver(db).List(context.Background(), modelCaller))
	if len(got) != 0 {
		t.Fatalf("ids = %v, want none — the model is granted to no project", got)
	}
}

// TestCatalogueModelGrantedToSelectedProjects is the whole point of the
// feature: the SAME row answers differently for two callers.
//
// Both halves are asserted with one row and two resolvers, because a test that
// only checked the granted project would pass against a gateway that ignored
// the field entirely.
func TestCatalogueModelGrantedToSelectedProjects(t *testing.T) {
	row := grantedRow("selected", modelShareScopeProjects, "["+modelCaller+"]")
	seed := func() *fakeModelDB {
		return &fakeModelDB{bySchema: map[string][]fakeModelRow{
			modelCaller: {},
			modelOther:  {},
			modelPublic: {row},
		}}
	}

	granted := modelIDs(newSharedResolver(seed()).List(context.Background(), modelCaller))
	if len(granted) != 1 || granted[0] != "selected" {
		t.Fatalf("the granted project got %v, want [selected]", granted)
	}
	// A DIFFERENT project, on the same row. The resolver caches per project, so
	// a second resolver is not needed for correctness — it is used so the two
	// answers cannot come from one cached list.
	other := modelIDs(newSharedResolver(seed()).List(context.Background(), modelOther))
	if len(other) != 0 {
		t.Fatalf("the ungranted project got %v, want none", other)
	}
}

// TestAnUngrantedModelCannotBeFetchedById is the dispatch half.
//
// Get is what the request path uses to turn the caller's `model` string into a
// row, so a grant enforced only in the listing would leave every id that had
// ever been published dispatchable for ever.
func TestAnUngrantedModelCannotBeFetchedById(t *testing.T) {
	db := &fakeModelDB{bySchema: map[string][]fakeModelRow{
		modelCaller: {},
		modelPublic: {grantedRow("selected", modelShareScopeProjects, "["+modelOther+"]")},
	}}

	if _, found := newSharedResolver(db).Get(context.Background(), modelCaller, "selected"); found {
		t.Fatal("Get answered a model this project holds no grant for")
	}
}

// TestTheCatalogueProjectKeepsItsOwnWithdrawnModels: the public project reads
// its own schema as its OWN scope, which is never filtered. Without that, a
// model set to `none` would disappear from the screen that has to set it back.
func TestTheCatalogueProjectKeepsItsOwnWithdrawnModels(t *testing.T) {
	db := &fakeModelDB{bySchema: map[string][]fakeModelRow{
		modelPublic: {grantedRow("withdrawn", "none", "")},
	}}

	got := modelIDs(newSharedResolver(db).List(context.Background(), modelPublic))
	if len(got) != 1 || got[0] != "withdrawn" {
		t.Fatalf("the catalogue project got %v, want [withdrawn] — its own rows are its own", got)
	}
}

// TestAModelWithNoGrantIsOfferedToEveryProject pins the compatibility rule.
// Every catalogue row written before the field existed carries neither key,
// and every one of them was offered to every project.
func TestAModelWithNoGrantIsOfferedToEveryProject(t *testing.T) {
	db := &fakeModelDB{bySchema: map[string][]fakeModelRow{
		modelCaller: {},
		modelPublic: {{title: "legacy-row", data: []byte(`{"name":"legacy-row"}`), shared: true}},
	}}

	got := modelIDs(newSharedResolver(db).List(context.Background(), modelCaller))
	if len(got) != 1 || got[0] != "legacy-row" {
		t.Fatalf("ids = %v, want [legacy-row] — an absent grant means every project", got)
	}
}

// TestAMalformedGrantIsReadAsEveryProject: a corrupt field must not become an
// outage. The write surface refuses these values where they are authored.
func TestAMalformedGrantIsReadAsEveryProject(t *testing.T) {
	cases := map[string][]byte{
		"a scope this gateway does not know": []byte(`{"name":"m","share_scope":"team"}`),
		"a scope that is not a string":       []byte(`{"name":"m","share_scope":7}`),
		"data that is not JSON at all":       []byte(`{{{`),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			db := &fakeModelDB{bySchema: map[string][]fakeModelRow{
				modelCaller: {},
				modelPublic: {{title: "m", data: data, shared: true}},
			}}
			got := modelIDs(newSharedResolver(db).List(context.Background(), modelCaller))
			if len(got) != 1 || got[0] != "m" {
				t.Fatalf("ids = %v, want [m]", got)
			}
		})
	}
}

// TestGrantIdsAreComparedAsNumbers: a client that stringified its project ids,
// or wrote one with a leading zero, still granted the project it named. A
// grant that failed to parse is a grant that silently does not apply.
func TestGrantIdsAreComparedAsNumbers(t *testing.T) {
	for _, spelling := range []string{`["7"]`, `["07"]`, `[7.0]`, `[9,7]`} {
		t.Run(spelling, func(t *testing.T) {
			db := &fakeModelDB{bySchema: map[string][]fakeModelRow{
				modelCaller: {},
				modelPublic: {grantedRow("m", modelShareScopeProjects, spelling)},
			}}
			got := modelIDs(newSharedResolver(db).List(context.Background(), modelCaller))
			if len(got) != 1 {
				t.Fatalf("shared_with %s gave %v, want the model to resolve for project %s",
					spelling, got, modelCaller)
			}
		})
	}
}

// TestGrantDoesNotMatchAnUnrelatedId is the negative of the reader above: a
// lenient parse must not become a lenient MATCH.
func TestGrantDoesNotMatchAnUnrelatedId(t *testing.T) {
	for _, spelling := range []string{`[]`, `[70]`, `["seven"]`, `"7"`, `[null]`} {
		t.Run(spelling, func(t *testing.T) {
			db := &fakeModelDB{bySchema: map[string][]fakeModelRow{
				modelCaller: {},
				modelPublic: {grantedRow("m", modelShareScopeProjects, spelling)},
			}}
			got := modelIDs(newSharedResolver(db).List(context.Background(), modelCaller))
			if len(got) != 0 {
				t.Fatalf("shared_with %s gave %v, want none", spelling, got)
			}
		})
	}
}
