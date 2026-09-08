package configurations

// The grant READER, which is deliberately lenient, and the grant DECISION,
// which is not.
//
// The two rules the cases below hold apart:
//
//   - anything the reader cannot understand is "every project", because every
//     catalogue row written before the field existed carries neither key and
//     every one of them was offered to every project. A stricter read would
//     withdraw them all on upgrade;
//   - a `projects` scope whose list is unreadable grants the row to NOBODY.
//     The scope says the grant is a list, so an unreadable list is a list this
//     project is not on. Widening it there would make a corrupt field the way
//     to obtain a model.

import "testing"

func TestReadModelGrantIsLenientAboutTheScope(t *testing.T) {
	t.Parallel()

	cases := map[string]map[string]any{
		"a nil document":                nil,
		"no scope at all":               {"name": "m"},
		"a scope this platform refuses": {"share_scope": "team"},
		"a scope that is not a string":  {"share_scope": 7},
		"the explicit all":              {"share_scope": "all"},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			grant := ReadModelGrant(data)
			if !grant.Allows(42) {
				t.Fatalf("%v was read as %#v, want every project", data, grant)
			}
		})
	}
}

func TestReadModelGrantWithdrawsTheNoneScope(t *testing.T) {
	t.Parallel()

	grant := ReadModelGrant(map[string]any{"share_scope": "none", "shared_with": []any{42.0}})
	if grant.Scope != ModelShareScopeNone {
		t.Fatalf("scope = %q, want none", grant.Scope)
	}
	// The list is IGNORED for this scope, not merged with it. A row that had
	// been granted to projects and was then withdrawn keeps the list until the
	// next save rewrites it, and reading it here would un-withdraw the model.
	if grant.Allows(42) {
		t.Error("a none-scoped model was offered to a project named in shared_with")
	}
}

func TestReadModelGrantReadsEverySpellingOfAProjectID(t *testing.T) {
	t.Parallel()

	cases := map[string]any{
		"a JSON number":            []any{42.0},
		"a stringified id":         []any{"42"},
		"a Go int":                 []any{42},
		"an id among others":       []any{7.0, 42.0, 9.0},
		"an id with a leading nil": []any{nil, 42.0},
	}
	for name, list := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			grant := ReadModelGrant(map[string]any{"share_scope": "projects", "shared_with": list})
			if !grant.Allows(42) {
				t.Fatalf("%v was read as %#v, want project 42 granted", list, grant)
			}
		})
	}
}

func TestReadModelGrantRefusesAProjectItDoesNotName(t *testing.T) {
	t.Parallel()

	cases := map[string]any{
		"an empty list":              []any{},
		"a list that is not a list":  "42",
		"an absent list":             nil,
		"another project":            []any{7.0},
		"an id that is not a number": []any{"forty-two"},
		"a fractional id":            []any{42.5},
		"a zero":                     []any{0.0},
		"a negative id":              []any{-42.0},
	}
	for name, list := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data := map[string]any{"share_scope": "projects"}
			if list != nil {
				data["shared_with"] = list
			}
			grant := ReadModelGrant(data)
			if grant.Allows(42) {
				t.Fatalf("%v was read as %#v, want project 42 refused", list, grant)
			}
		})
	}
}

func TestIsSupportedModelShareScope(t *testing.T) {
	t.Parallel()

	for _, scope := range []ModelShareScope{ModelShareScopeAll, ModelShareScopeNone, ModelShareScopeProjects} {
		if !IsSupportedModelShareScope(scope) {
			t.Errorf("%q is not supported", scope)
		}
	}
	// The WRITE side is strict where the read side is lenient. An unknown value
	// is stored and then read back as "all", so admitting it would grant a model
	// to every project while the screen that wrote it said otherwise.
	for _, scope := range []ModelShareScope{"", "team", "ALL", "project"} {
		if IsSupportedModelShareScope(scope) {
			t.Errorf("%q was admitted as a scope this platform writes", scope)
		}
	}
}

// TestTheCatalogueMergeAppliesTheGrant is the builder's own half: the filter
// runs on the PUBLIC candidates and on nothing else.
func TestTheCatalogueMergeAppliesTheGrant(t *testing.T) {
	t.Parallel()

	own := []CurrentModelCatalogItem{{
		Name: "own", ProjectID: 5,
		// An own row carrying a withdrawing grant. It must survive: the row is
		// this project's own and no grant was ever needed for it.
		Grant: ModelGrant{Scope: ModelShareScopeNone},
	}}
	shared := []CurrentModelCatalogItem{
		{Name: "to-all", ProjectID: 1, Shared: true, Grant: ModelGrant{Scope: ModelShareScopeAll}},
		{Name: "to-none", ProjectID: 1, Shared: true, Grant: ModelGrant{Scope: ModelShareScopeNone}},
		{Name: "to-five", ProjectID: 1, Shared: true,
			Grant: ModelGrant{Scope: ModelShareScopeProjects, Projects: []int32{5}}},
		{Name: "to-six", ProjectID: 1, Shared: true,
			Grant: ModelGrant{Scope: ModelShareScopeProjects, Projects: []int32{6}}},
	}

	response := BuildCurrentModelCatalog(CurrentModelCatalogRequest{
		Section:           CurrentModelSectionLLM,
		ProjectID:         5,
		PublicProjectID:   1,
		IncludeShared:     true,
		ProjectItems:      own,
		PublicSharedItems: shared,
	})

	got := map[string]bool{}
	for _, item := range response.Items {
		got[item.Name] = true
	}
	for _, want := range []string{"own", "to-all", "to-five"} {
		if !got[want] {
			t.Errorf("%q is missing from %v", want, got)
		}
	}
	for _, unwanted := range []string{"to-none", "to-six"} {
		if got[unwanted] {
			t.Errorf("%q reached a project that holds no grant for it", unwanted)
		}
	}
	if response.Total != len(response.Items) {
		t.Errorf("total = %d, items = %d — the count must describe the filtered set",
			response.Total, len(response.Items))
	}
}
