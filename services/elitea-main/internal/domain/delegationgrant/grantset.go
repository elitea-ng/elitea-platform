// Package delegationgrant is the delegation grant of ADR-0044 amendment 1:
// attaching a callee X to a caller Y creates a grant for the edge Y to X that
// lists what X holds, pinned to the digest of that set. The worker reads the
// approved grant as G(Y to X) in E(X) = own(X) ∩ (E(Y) ∪ G(Y to X)) ∩ P.
//
// This file is the grant SET: its canonical sorted form, its digest and the
// containment checks. The canonical form and the digest are shared with the
// worker (libs/rust/agent-runtime/src/delegation.rs, GrantSet::canonical and
// GrantSet::digest). Both sides run the vectors in
// libs/proto/contracts/delegation-grant-digest-vectors-v1.json and must
// produce the same bytes, so a grant approved in Main is the one the worker
// pins.
package delegationgrant

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Schema is the canonical form's schema tag, the one the worker writes.
const Schema = "elitea.delegation.grants.v1"

// The bounds the worker applies when it builds a set; a stored set past them
// is refused, so a set Main stores is one the worker can read whole.
const (
	MaxToolkits  = 1024
	MaxToolNames = 1024
	MaxHosts     = 256
	MaxClasses   = 32
)

// ToolMode is how a toolkit's tools are selected.
type ToolMode string

const (
	// ToolsAllExcept: every tool but the named ones.
	ToolsAllExcept ToolMode = "all_except"
	// ToolsOnly: only the named tools.
	ToolsOnly ToolMode = "only"
)

// Tools is the tool selection of one toolkit.
type Tools struct {
	Mode  ToolMode
	Names []string
}

// Allows reports whether the selection covers tool.
func (t Tools) Allows(tool string) bool {
	found := contains(t.Names, tool)
	if t.Mode == ToolsOnly {
		return found
	}
	return !found
}

// ToolkitGrant is one toolkit of a grant set. Key is "toolkit:<id>" for a
// toolkit row and "builtin:<type>" for a built-in toolkit with none (the
// worker's key). Credential is the toolkit's credential reference id, empty
// for none.
type ToolkitGrant struct {
	Key        string
	Tools      Tools
	Credential string
}

// GrantSet is what a callee holds, or what an edge's grant covers.
//
// DataClasses is nil when the set imposes no data class (the worker's
// None); an empty non-nil slice is normalised to nil, as the worker does.
type GrantSet struct {
	Toolkits    []ToolkitGrant
	DataClasses []string
	EgressHosts []string
}

// Normalize returns the set in its canonical order with duplicates removed:
// toolkits by key bytes, names and hosts and classes sorted, an empty data
// class list nil. Where a key repeats the last entry wins, as the worker's
// map insert does.
func (s GrantSet) Normalize() GrantSet {
	byKey := make(map[string]ToolkitGrant, len(s.Toolkits))
	for _, toolkit := range s.Toolkits {
		toolkit.Tools = Tools{Mode: toolkit.Tools.Mode, Names: sortedUnique(toolkit.Tools.Names)}
		byKey[toolkit.Key] = toolkit
	}
	out := GrantSet{Toolkits: make([]ToolkitGrant, 0, len(byKey))}
	for _, toolkit := range byKey {
		out.Toolkits = append(out.Toolkits, toolkit)
	}
	sort.Slice(out.Toolkits, func(i, j int) bool { return out.Toolkits[i].Key < out.Toolkits[j].Key })
	if classes := sortedUnique(s.DataClasses); len(classes) > 0 {
		out.DataClasses = classes
	}
	out.EgressHosts = sortedUnique(s.EgressHosts)
	return out
}

// Canonical is the canonical JSON the digest is computed over: compact, every
// object's members sorted by key bytes, strings escaped as serde_json writes
// them (never the HTML escapes of encoding/json). It is byte for byte the
// worker's crate::canonical::to_vec of GrantSet::canonical.
func (s GrantSet) Canonical() []byte {
	n := s.Normalize()
	var b strings.Builder
	b.WriteString(`{"data_classes":`)
	if n.DataClasses == nil {
		b.WriteString("null")
	} else {
		writeStrings(&b, n.DataClasses)
	}
	b.WriteString(`,"egress_hosts":`)
	writeStrings(&b, n.EgressHosts)
	b.WriteString(`,"schema":`)
	writeString(&b, Schema)
	b.WriteString(`,"toolkits":[`)
	for i, toolkit := range n.Toolkits {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"credential":`)
		if toolkit.Credential == "" {
			b.WriteString("null")
		} else {
			writeString(&b, toolkit.Credential)
		}
		b.WriteString(`,"key":`)
		writeString(&b, toolkit.Key)
		b.WriteString(`,"tools":{`)
		writeString(&b, string(toolkit.Tools.Mode))
		b.WriteByte(':')
		writeStrings(&b, toolkit.Tools.Names)
		b.WriteString("}}")
	}
	b.WriteString("]}")
	return []byte(b.String())
}

// Digest is the lowercase hex SHA-256 of Canonical, the value a delegation
// step records as the grant digest.
func (s GrantSet) Digest() string {
	sum := sha256.Sum256(s.Canonical())
	return hex.EncodeToString(sum[:])
}

// MarshalJSON writes the canonical form, so a set stored as jsonb is the
// document the worker parses.
func (s GrantSet) MarshalJSON() ([]byte, error) { return s.Canonical(), nil }

// UnmarshalJSON reads the canonical form (see ParseGrantSet).
func (s *GrantSet) UnmarshalJSON(raw []byte) error {
	parsed, err := ParseGrantSet(raw)
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// ErrMalformedSet is a stored or submitted set that is not a grant set.
var ErrMalformedSet = errors.New("delegation grant: malformed grant set")

type wireSet struct {
	Schema      string     `json:"schema"`
	Toolkits    []wireTool `json:"toolkits"`
	DataClasses []string   `json:"data_classes"`
	EgressHosts []string   `json:"egress_hosts"`
}

type wireTool struct {
	Key        string              `json:"key"`
	Tools      map[string][]string `json:"tools"`
	Credential *string             `json:"credential"`
}

// ParseGrantSet reads the canonical form. It refuses a set past the bounds or
// with a malformed toolkit, and returns it normalised.
func ParseGrantSet(raw []byte) (GrantSet, error) {
	var wire wireSet
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return GrantSet{}, fmt.Errorf("%w: %v", ErrMalformedSet, err)
	}
	if wire.Schema != Schema {
		return GrantSet{}, fmt.Errorf("%w: schema %q", ErrMalformedSet, wire.Schema)
	}
	if len(wire.Toolkits) > MaxToolkits || len(wire.EgressHosts) > MaxHosts || len(wire.DataClasses) > MaxClasses {
		return GrantSet{}, fmt.Errorf("%w: past the bounds", ErrMalformedSet)
	}
	set := GrantSet{DataClasses: wire.DataClasses, EgressHosts: wire.EgressHosts}
	for _, toolkit := range wire.Toolkits {
		grant, err := parseToolkit(toolkit)
		if err != nil {
			return GrantSet{}, err
		}
		set.Toolkits = append(set.Toolkits, grant)
	}
	return set.Normalize(), nil
}

func parseToolkit(toolkit wireTool) (ToolkitGrant, error) {
	if len(toolkit.Tools) != 1 || !validToolkitKey(toolkit.Key) {
		return ToolkitGrant{}, fmt.Errorf("%w: toolkit %q", ErrMalformedSet, toolkit.Key)
	}
	grant := ToolkitGrant{Key: toolkit.Key}
	for mode, names := range toolkit.Tools {
		switch ToolMode(mode) {
		case ToolsAllExcept, ToolsOnly:
			grant.Tools = Tools{Mode: ToolMode(mode), Names: names}
		default:
			return ToolkitGrant{}, fmt.Errorf("%w: toolkit %q tool mode %q", ErrMalformedSet, toolkit.Key, mode)
		}
	}
	if len(grant.Tools.Names) > MaxToolNames {
		return ToolkitGrant{}, fmt.Errorf("%w: toolkit %q has too many tools", ErrMalformedSet, toolkit.Key)
	}
	if toolkit.Credential != nil {
		grant.Credential = *toolkit.Credential
	}
	return grant, nil
}

func validToolkitKey(key string) bool {
	for _, prefix := range []string{"toolkit:", "builtin:"} {
		if rest, ok := strings.CutPrefix(key, prefix); ok && rest != "" {
			return true
		}
	}
	return false
}

// Item kinds a set is itemised into, for the review dialog, a partial
// approval and the missing list.
const (
	ItemToolkit    = "toolkit"
	ItemTool       = "tool"
	ItemCredential = "credential"
	ItemDataClass  = "data_class"
	ItemEgressHost = "egress_host"
)

// Item is one thing a grant set lists. Toolkit names the toolkit a tool or
// credential belongs to; Name is the toolkit key, tool name, credential
// reference id, data class or host.
type Item struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Toolkit string `json:"toolkit,omitempty"`
}

// Items lists what the set holds, in a stable order: toolkits (each followed
// by its credential and, for an `only` selection, its tools), data classes,
// hosts. A toolkit that selects every tool lists no tool items: it holds them
// all by listing the toolkit.
func (s GrantSet) Items() []Item {
	n := s.Normalize()
	var items []Item
	for _, toolkit := range n.Toolkits {
		items = append(items, Item{Kind: ItemToolkit, Name: toolkit.Key})
		if toolkit.Credential != "" {
			items = append(items, Item{Kind: ItemCredential, Name: toolkit.Credential, Toolkit: toolkit.Key})
		}
		if toolkit.Tools.Mode == ToolsOnly {
			for _, name := range toolkit.Tools.Names {
				items = append(items, Item{Kind: ItemTool, Name: name, Toolkit: toolkit.Key})
			}
		}
	}
	for _, class := range n.DataClasses {
		items = append(items, Item{Kind: ItemDataClass, Name: class})
	}
	for _, host := range n.EgressHosts {
		items = append(items, Item{Kind: ItemEgressHost, Name: host})
	}
	return items
}

// Missing lists what current holds that s does not cover, the items a run
// narrows out when current has drifted past the grant s. The check is set
// containment, never version equality: a current set that is the same or
// smaller returns nothing.
//
//   - a toolkit s does not list is missing as a toolkit;
//   - a toolkit whose credential reference differs from s's is missing as a
//     credential (the toolkit cannot run on a credential nobody approved);
//   - a tool s's selection does not cover is missing as a tool;
//   - a toolkit that selects every tool, where s approved only some, is
//     narrowed to the approved tools, not removed: it is reported as the one
//     item {tool, "*"} so a reader sees the breadth was cut;
//   - a data class or host s does not list is missing.
func (s GrantSet) Missing(current GrantSet) []Item {
	granted := s.Normalize()
	held := make(map[string]ToolkitGrant, len(granted.Toolkits))
	for _, toolkit := range granted.Toolkits {
		held[toolkit.Key] = toolkit
	}
	cur := current.Normalize()
	var missing []Item
	for _, toolkit := range cur.Toolkits {
		approved, ok := held[toolkit.Key]
		if !ok {
			missing = append(missing, Item{Kind: ItemToolkit, Name: toolkit.Key})
			continue
		}
		if toolkit.Credential != "" && toolkit.Credential != approved.Credential {
			missing = append(missing, Item{Kind: ItemCredential, Name: toolkit.Credential, Toolkit: toolkit.Key})
			continue
		}
		missing = append(missing, missingTools(toolkit, approved)...)
	}
	missing = append(missing, notIn(cur.DataClasses, granted.DataClasses, ItemDataClass)...)
	missing = append(missing, notIn(cur.EgressHosts, granted.EgressHosts, ItemEgressHost)...)
	return missing
}

func missingTools(current, approved ToolkitGrant) []Item {
	var missing []Item
	switch {
	case current.Tools.Mode == ToolsOnly:
		for _, name := range current.Tools.Names {
			if !approved.Tools.Allows(name) {
				missing = append(missing, Item{Kind: ItemTool, Name: name, Toolkit: current.Key})
			}
		}
	case approved.Tools.Mode == ToolsOnly:
		missing = append(missing, Item{Kind: ItemTool, Name: "*", Toolkit: current.Key})
	default:
		// Every tool but E_cur against every tool but E_app: a tool the
		// grant excluded and the current set no longer excludes is new.
		for _, name := range approved.Tools.Names {
			if !contains(current.Tools.Names, name) {
				missing = append(missing, Item{Kind: ItemTool, Name: name, Toolkit: current.Key})
			}
		}
	}
	return missing
}

func notIn(values, allowed []string, kind string) []Item {
	var missing []Item
	for _, value := range values {
		if !contains(allowed, value) {
			missing = append(missing, Item{Kind: kind, Name: value})
		}
	}
	return missing
}

// Contains reports whether s covers everything current holds.
func (s GrantSet) Contains(current GrantSet) bool { return len(s.Missing(current)) == 0 }

// Restrict keeps only the items of s that appear in keep: a toolkit only when
// its toolkit item is kept AND its credential item is kept (a toolkit cannot
// run without the credential it names); the tools of an `only` selection only
// when each tool item is kept; data classes and hosts when kept. A toolkit
// left with an empty `only` selection is dropped.
func (s GrantSet) Restrict(keep []Item) GrantSet {
	kept := make(map[Item]bool, len(keep))
	for _, item := range keep {
		kept[item] = true
	}
	n := s.Normalize()
	out := GrantSet{}
	for _, toolkit := range n.Toolkits {
		if !kept[Item{Kind: ItemToolkit, Name: toolkit.Key}] {
			continue
		}
		if toolkit.Credential != "" && !kept[Item{Kind: ItemCredential, Name: toolkit.Credential, Toolkit: toolkit.Key}] {
			continue
		}
		if toolkit.Tools.Mode == ToolsOnly {
			var tools []string
			for _, name := range toolkit.Tools.Names {
				if kept[Item{Kind: ItemTool, Name: name, Toolkit: toolkit.Key}] {
					tools = append(tools, name)
				}
			}
			if len(tools) == 0 {
				continue
			}
			toolkit.Tools.Names = tools
		}
		out.Toolkits = append(out.Toolkits, toolkit)
	}
	for _, class := range n.DataClasses {
		if kept[Item{Kind: ItemDataClass, Name: class}] {
			out.DataClasses = append(out.DataClasses, class)
		}
	}
	for _, host := range n.EgressHosts {
		if kept[Item{Kind: ItemEgressHost, Name: host}] {
			out.EgressHosts = append(out.EgressHosts, host)
		}
	}
	return out.Normalize()
}

// Without lists the items of s that approved does not hold: what an approval
// left out.
func (s GrantSet) Without(approved GrantSet) []Item {
	have := make(map[Item]bool)
	for _, item := range approved.Items() {
		have[item] = true
	}
	left := []Item{}
	for _, item := range s.Items() {
		if !have[item] {
			left = append(left, item)
		}
	}
	return left
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func sortedUnique(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	unique := out[:0]
	for i, value := range out {
		if i == 0 || value != out[i-1] {
			unique = append(unique, value)
		}
	}
	return unique
}

func writeStrings(b *strings.Builder, values []string) {
	b.WriteByte('[')
	for i, value := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		writeString(b, value)
	}
	b.WriteByte(']')
}

// writeString writes value as serde_json does: the quote and the backslash
// escaped, \b \f \n \r \t by their short form, every other control character
// as \u00xx with lowercase hex, everything else (including U+2028 and the
// HTML characters) raw. Invalid UTF-8 cannot reach a Rust string, so it is
// written as U+FFFD (what ranging over a Go string yields).
func writeString(b *strings.Builder, value string) {
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}
