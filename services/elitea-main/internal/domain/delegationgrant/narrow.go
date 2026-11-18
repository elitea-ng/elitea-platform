package delegationgrant

import "sort"

// The delegation rule of a composition edge, for the one place where Main (not
// the worker) enforces it: the nested agent a desktop runs as a cloud child
// turn (issue 2214). It mirrors the worker's rule over toolkits, tools and
// credentials, libs/rust/agent-runtime/src/delegation.rs
// (DelegationScope::delegate_edge) and delegation/grants.rs
// (narrow_with_grant):
//
//	E(X) = own(X) ∩ (E(Y) ∪ G(Y→X)) ∩ P
//
// P is not applied here: the child's own command carries the run principal's
// bound and the worker applies it to the child's root (own ∩ P), so the
// intersection with it happens exactly once, on the same switch as for every
// other run. Data classes and egress hosts are not narrowed here either: a
// data class lives on the agent's registry row and the egress allowlist on the
// command, neither in the frozen tool list this narrows.

// EdgeLabels names the toolkits of a set for a notice: toolkit key to display
// name. A key it does not name is shown as the key.
type EdgeLabels map[string]string

func (l EdgeLabels) of(key string) string {
	if name := l[key]; name != "" {
		return name
	}
	return key
}

// NarrowWithGrant is own(X) ∩ (E(Y) ∪ G): the callee's toolkits cut to what the
// caller's set or the approved grant covers, and what was cut. A callee whose
// toolkit neither holds is narrowed out (a toolkit item), a credential neither
// holds drops its toolkit (a credential item), and a tool selection is cut to
// the union of the two selections (tool items; one `*` item for a toolkit that
// selected every tool and is now cut to some).
func NarrowWithGrant(caller, grant, requested GrantSet, labels EdgeLabels) (GrantSet, []Item) {
	callerHeld := toolkitIndex(caller)
	grantHeld := toolkitIndex(grant)
	callerCredentials := map[string]bool{}
	for _, toolkit := range callerHeld {
		if toolkit.Credential != "" {
			callerCredentials[toolkit.Credential] = true
		}
	}
	var narrowed []Item
	var kept []ToolkitGrant
	for _, want := range requested.Normalize().Toolkits {
		label := labels.of(want.Key)
		held, ok := heldSelection(callerHeld, grantHeld, want.Key)
		if !ok {
			narrowed = append(narrowed, Item{Kind: ItemToolkit, Name: label})
			continue
		}
		if want.Credential != "" && !callerCredentials[want.Credential] && grantHeld[want.Key].Credential != want.Credential {
			narrowed = append(narrowed, Item{Kind: ItemCredential, Name: want.Credential, Toolkit: label})
			continue
		}
		selection := held.intersect(want.Tools)
		narrowed = append(narrowed, cutTools(want.Tools, selection, label)...)
		if selection.Mode == ToolsOnly && len(selection.Names) == 0 {
			continue
		}
		want.Tools = selection
		kept = append(kept, want)
	}
	sortItems(narrowed)
	return GrantSet{Toolkits: kept}.Normalize(), narrowed
}

// NarrowToCaller is the rule of an edge with no approved grant: own(X) ∩ E(Y).
// The callee's toolkit selections are cut to the caller's; what the caller does
// not hold at all is reported by RefuseWithoutGrant first.
func NarrowToCaller(caller, requested GrantSet) GrantSet {
	callerHeld := toolkitIndex(caller)
	var kept []ToolkitGrant
	for _, want := range requested.Normalize().Toolkits {
		held, ok := callerHeld[want.Key]
		if !ok {
			// Only report mode reaches here: the callee keeps it unenforced.
			kept = append(kept, want)
			continue
		}
		want.Tools = held.Tools.intersect(want.Tools)
		if want.Tools.Mode == ToolsOnly && len(want.Tools.Names) == 0 {
			continue
		}
		kept = append(kept, want)
	}
	return GrantSet{Toolkits: kept}.Normalize()
}

// RefuseWithoutGrant is the first thing the callee holds that its caller does
// not, for an edge with no approved grant: toolkits, then credentials, then the
// tools of a selection the callee names one by one. Nothing is the callee
// holding no more than its caller.
func RefuseWithoutGrant(caller, requested GrantSet, labels EdgeLabels) (Item, bool) {
	callerHeld := toolkitIndex(caller)
	wanted := requested.Normalize().Toolkits
	for _, want := range wanted {
		if _, ok := callerHeld[want.Key]; !ok {
			return Item{Kind: ItemToolkit, Name: labels.of(want.Key)}, true
		}
	}
	credentials := map[string]bool{}
	for _, toolkit := range callerHeld {
		if toolkit.Credential != "" {
			credentials[toolkit.Credential] = true
		}
	}
	for _, want := range wanted {
		if want.Credential != "" && !credentials[want.Credential] {
			return Item{Kind: ItemCredential, Name: want.Credential, Toolkit: labels.of(want.Key)}, true
		}
	}
	for _, want := range wanted {
		if want.Tools.Mode != ToolsOnly {
			continue
		}
		for _, name := range want.Tools.Names {
			if !callerHeld[want.Key].Tools.Allows(name) {
				return Item{Kind: ItemTool, Name: name, Toolkit: labels.of(want.Key)}, true
			}
		}
	}
	return Item{}, false
}

func toolkitIndex(set GrantSet) map[string]ToolkitGrant {
	index := make(map[string]ToolkitGrant, len(set.Toolkits))
	for _, toolkit := range set.Normalize().Toolkits {
		index[toolkit.Key] = toolkit
	}
	return index
}

// heldSelection is what E(Y) ∪ G lets the toolkit use: the union of the
// caller's selection and the grant's, or false when neither holds the toolkit.
func heldSelection(caller, grant map[string]ToolkitGrant, key string) (Tools, bool) {
	fromCaller, inCaller := caller[key]
	fromGrant, inGrant := grant[key]
	switch {
	case inCaller && inGrant:
		return fromCaller.Tools.union(fromGrant.Tools), true
	case inCaller:
		return fromCaller.Tools, true
	case inGrant:
		return fromGrant.Tools, true
	}
	return Tools{}, false
}

// cutTools records the tools the cut removed: each tool of an `only` selection
// that is not left, or, for a selection of every tool cut to some, one `*`.
func cutTools(wanted, kept Tools, label string) []Item {
	item := func(name string) Item { return Item{Kind: ItemTool, Name: name, Toolkit: label} }
	var out []Item
	switch {
	case wanted.Mode == ToolsOnly:
		for _, name := range wanted.Names {
			if !kept.Allows(name) {
				out = append(out, item(name))
			}
		}
	case kept.Mode == ToolsOnly:
		out = append(out, item("*"))
	default:
		for _, name := range kept.Names {
			if !contains(wanted.Names, name) {
				out = append(out, item(name))
			}
		}
	}
	return out
}

func (t Tools) intersect(other Tools) Tools {
	switch {
	case t.Mode == ToolsOnly && other.Mode == ToolsOnly:
		var names []string
		for _, name := range t.Names {
			if contains(other.Names, name) {
				names = append(names, name)
			}
		}
		return Tools{Mode: ToolsOnly, Names: sortedUnique(names)}
	case t.Mode == ToolsOnly:
		return Tools{Mode: ToolsOnly, Names: without(t.Names, other.Names)}
	case other.Mode == ToolsOnly:
		return Tools{Mode: ToolsOnly, Names: without(other.Names, t.Names)}
	}
	return Tools{Mode: ToolsAllExcept, Names: sortedUnique(append(append([]string(nil), t.Names...), other.Names...))}
}

func (t Tools) union(other Tools) Tools {
	switch {
	case t.Mode == ToolsOnly && other.Mode == ToolsOnly:
		return Tools{Mode: ToolsOnly, Names: sortedUnique(append(append([]string(nil), t.Names...), other.Names...))}
	case t.Mode == ToolsOnly:
		return Tools{Mode: ToolsAllExcept, Names: without(other.Names, t.Names)}
	case other.Mode == ToolsOnly:
		return Tools{Mode: ToolsAllExcept, Names: without(t.Names, other.Names)}
	}
	var names []string
	for _, name := range t.Names {
		if contains(other.Names, name) {
			names = append(names, name)
		}
	}
	return Tools{Mode: ToolsAllExcept, Names: sortedUnique(names)}
}

// without is the names of from that are not in drop, sorted.
func without(from, drop []string) []string {
	var out []string
	for _, name := range from {
		if !contains(drop, name) {
			out = append(out, name)
		}
	}
	return sortedUnique(out)
}

func sortItems(items []Item) {
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Toolkit < b.Toolkit
	})
}
