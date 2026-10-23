// Package applicationcomposition refuses, at save time, a saved composition of
// agents and pipelines that the Worker would refuse at run time (FU-244): a
// circular reference, nesting past the Worker's tier, depth or scope bounds,
// the hop budget, or a pipeline that pauses (interrupt_before/after) used as an
// Agent's tool beneath a pipeline root, whose pause that root cannot continue.
//
// The walk mirrors the Worker's assembly path
// (services/elitea-worker-rust src/agents/application_tools/materialization.rs
// ApplicationMaterializationPath, src/agents/pipeline/composition.rs
// collect_paths, src/agents/pipeline.rs admit_saved_pipeline_child) and its
// bounds are the shared contract
// libs/proto/contracts/application-composition-limits-v1.json. The version
// being saved is checked as the root of its own kind: embedding it anywhere
// only adds depth, so a violation found here is one in every root that can
// reach it, and the save never refuses what the runtime would run. An
// attachment the walk cannot read (a deleted version) is not followed: the
// Worker leaves such an attachment out and names it, it does not refuse.
package applicationcomposition

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The Worker's composition bounds (application-composition-limits-v1.json).
const (
	// MaxAgentTiers is the most agent tiers on one path, the root agent included.
	MaxAgentTiers = 3
	// MaxPipelineDepth bounds saved pipeline nesting (see enterPipeline and
	// the agent-node rule in walkPipeline).
	MaxPipelineDepth = 3
	// MaxOrdinaryGraphScopes is the most pipeline Agent-node levels on one path.
	MaxOrdinaryGraphScopes = 2
	// MaxApplicationHops is the most attached agents and pipelines one run builds.
	MaxApplicationHops = 25
)

// Code is the public refusal code; the Worker's run-time refusal maps to it too.
const Code = "APPLICATION_COMPOSITION_INVALID"

// RuntimeMessage is the registered, data-free text of a run refused for its
// stored composition (application-composition-limits-v1.json runtime_message):
// the Worker sends it, and Main's start refusal (ErrAtStart) answers it.
const RuntimeMessage = "This agent or pipeline cannot run: its attachments form a circular reference, nest deeper than allowed, or include a pipeline that pauses where the pause cannot be continued. Review its attachments and change the one that breaks the rule."

// ErrAtStart refuses a run whose stored composition Main's start validation
// rejects for a bound (a circular reference, the hop or tier bound): a
// composition saved before the save check, or broken later by an edit of a
// descendant. Data-free; the save check names the attachment.
var ErrAtStart error = &apierr.APIError{Status: http.StatusUnprocessableEntity, Code: Code, Message: RuntimeMessage}

// Bound names which rule a composition broke; data-free.
type Bound string

const (
	BoundCycle         Bound = "cycle"
	BoundAgentTiers    Bound = "agent_tiers"
	BoundPipelineDepth Bound = "pipeline_depth"
	BoundGraphScopes   Bound = "ordinary_graph_scopes"
	BoundHops          Bound = "application_hops"
	BoundStaticPause   Bound = "static_pause_under_graph_root"
)

// Kind is a saved version's kind.
type Kind int

const (
	KindAgent Kind = iota
	KindPipeline
)

// Identity is one saved version.
type Identity struct {
	ApplicationID int64
	VersionID     int64
}

// Attachment is one agent or pipeline attached to a version as a tool, under
// the alias a pipeline's Agent node selects it by.
type Attachment struct {
	Alias    string
	Identity Identity
}

// AgentNode is one pipeline `agent` node and the attachment alias it runs.
type AgentNode struct {
	ID    string
	Alias string
}

// Version is what the walk reads of one saved version.
type Version struct {
	Kind        Kind
	Name        string
	Attachments []Attachment
	// Pipeline only, from ReadPipeline.
	AgentNodes  []AgentNode
	StaticPause bool
}

// Source reads one saved version. ok is false for a version that does not
// exist (or cannot be read as a composition); the walk does not follow it.
type Source interface {
	Version(ctx context.Context, id Identity) (version Version, ok bool, err error)
}

// Violation is a refused composition; Error is the readable refusal naming
// the attachment and the path that reaches it.
type Violation struct {
	Bound   Bound
	message string
}

func (v *Violation) Error() string { return v.message }

// Refusal answers the 400 a save writes for err when err is a Violation, ErrAtStart
// for a refused start, else nil.
func Refusal(err error) *apierr.APIError {
	if errors.Is(err, ErrAtStart) {
		return ErrAtStart.(*apierr.APIError)
	}
	var violation *Violation
	if !errors.As(err, &violation) {
		return nil
	}
	return &apierr.APIError{Status: http.StatusBadRequest, Code: Code, Message: violation.message}
}

// Check walks the composition of root as a root of its own kind. root is read
// through source unless override is non-nil (the version as it is about to be
// saved). A Violation is the refusal; any other error is a read failure.
func Check(ctx context.Context, source Source, root Identity, override *Version) error {
	version := override
	if version == nil {
		read, ok, err := source.Version(ctx, root)
		if err != nil || !ok {
			return err
		}
		version = &read
	}
	w := walker{source: source, cache: map[Identity]Version{}}
	start := path{ancestors: []Identity{root}, trail: []string{label(version.Name)}, tier: 1}
	if version.Kind == KindPipeline {
		start.depth = 1
		start.graphRoot = true
		return w.walkPipeline(ctx, *version, start, 0)
	}
	return w.walkAgent(ctx, *version, start)
}

// path is one branch's immutable ancestry; hops are shared by the walker.
type path struct {
	ancestors []Identity
	trail     []string
	tier      int
	depth     int
	scopes    int
	graphRoot bool
}

func (p path) with(id Identity, name string) path {
	next := p
	next.ancestors = append(append([]Identity(nil), p.ancestors...), id)
	next.trail = append(append([]string(nil), p.trail...), label(name))
	return next
}

type walker struct {
	source Source
	cache  map[Identity]Version
	hops   int
}

func (w *walker) read(ctx context.Context, id Identity) (Version, bool, error) {
	if version, ok := w.cache[id]; ok {
		return version, true, nil
	}
	version, ok, err := w.source.Version(ctx, id)
	if err != nil || !ok {
		return Version{}, false, err
	}
	w.cache[id] = version
	return version, true, nil
}

// enter mirrors ApplicationMaterializationPath::enter: cycle, then the hop budget.
func (w *walker) enter(p path, id Identity, child Version) (path, error) {
	for _, ancestor := range p.ancestors {
		if ancestor == id {
			return path{}, violation(BoundCycle, fmt.Sprintf(
				"These attachments would create a circular agent reference: %s. Remove the circular dependency first.",
				chain(p.trail, child.Name)))
		}
	}
	if w.hops >= MaxApplicationHops {
		return path{}, violation(BoundHops, fmt.Sprintf(
			"This agent or pipeline reaches more than %d attached agents and pipelines (at %s). Remove attachments before saving.",
			MaxApplicationHops, chain(p.trail, child.Name)))
	}
	w.hops++
	return p.with(id, child.Name), nil
}

func (w *walker) enterAgent(p path, id Identity, child Version) (path, error) {
	if p.tier >= MaxAgentTiers {
		return path{}, violation(BoundAgentTiers, fmt.Sprintf(
			"Agents are nested more than %d levels deep: %s. Attach fewer levels of agents before saving.",
			MaxAgentTiers, chain(p.trail, child.Name)))
	}
	next, err := w.enter(p, id, child)
	next.tier++
	return next, err
}

func (w *walker) enterPipeline(p path, id Identity, child Version) (path, error) {
	if p.depth > MaxPipelineDepth {
		return path{}, depthViolation(chain(p.trail, child.Name))
	}
	next, err := w.enter(p, id, child)
	next.depth++
	return next, err
}

// walkAgent follows every attachment of an agent, as an Agent's assembly builds them.
func (w *walker) walkAgent(ctx context.Context, agent Version, p path) error {
	seen := map[Identity]bool{}
	for _, attachment := range agent.Attachments {
		child, ok, err := w.read(ctx, attachment.Identity)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if child.Kind == KindAgent {
			// The Worker builds each attached agent once (application_references).
			if seen[attachment.Identity] {
				continue
			}
			seen[attachment.Identity] = true
			next, err := w.enterAgent(p, attachment.Identity, child)
			if err != nil {
				return err
			}
			if err := w.walkAgent(ctx, child, next); err != nil {
				return err
			}
			continue
		}
		next, err := w.enterPipeline(p, attachment.Identity, child)
		if err != nil {
			return err
		}
		if p.graphRoot {
			if paused, err := w.pauses(ctx, child, map[Identity]bool{attachment.Identity: true}); err != nil {
				return err
			} else if paused {
				return violation(BoundStaticPause, fmt.Sprintf(
					"Pipeline %s pauses before or after a node, so it cannot be a tool of agent %s inside a pipeline: an Agent node cannot continue that pause (%s). Remove the pause or the attachment before saving.",
					label(child.Name), label(agent.Name), chain(p.trail, child.Name)))
			}
		}
		if err := w.walkPipeline(ctx, child, next, 0); err != nil {
			return err
		}
	}
	return nil
}

// walkPipeline follows a pipeline's Agent nodes, as a graph's assembly binds them.
// composition is the saved-pipeline nesting below the nearest pipeline root or tool.
func (w *walker) walkPipeline(ctx context.Context, pipeline Version, p path, composition int) error {
	if composition == MaxPipelineDepth && len(pipeline.AgentNodes) > 0 {
		return depthViolation(chain(p.trail, "Agent node "+label(pipeline.AgentNodes[0].ID)))
	}
	bound := map[string]bool{}
	for _, node := range pipeline.AgentNodes {
		if bound[node.Alias] {
			continue
		}
		bound[node.Alias] = true
		attachment, ok := attached(pipeline, node.Alias)
		if !ok {
			continue
		}
		child, ok, err := w.read(ctx, attachment.Identity)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if child.Kind == KindPipeline {
			next, err := w.enterPipeline(p, attachment.Identity, child)
			if err != nil {
				return err
			}
			if err := w.walkPipeline(ctx, child, next, composition+1); err != nil {
				return err
			}
			continue
		}
		if p.scopes >= MaxOrdinaryGraphScopes {
			return violation(BoundGraphScopes, fmt.Sprintf(
				"Agent node %s of pipeline %s runs agent %s as a third level of pipeline Agent nodes (%s); at most %d levels are supported. Reduce the nesting before saving.",
				label(node.ID), label(pipeline.Name), label(child.Name), chain(p.trail, child.Name), MaxOrdinaryGraphScopes))
		}
		scoped := p
		scoped.scopes++
		next, err := w.enterAgent(scoped, attachment.Identity, child)
		if err != nil {
			return err
		}
		if err := w.walkAgent(ctx, child, next); err != nil {
			return err
		}
	}
	return nil
}

// pauses answers whether a pipeline, or a pipeline its Agent nodes run, has a
// static pause (the Worker's guarded "static continuation" kind).
func (w *walker) pauses(ctx context.Context, pipeline Version, visiting map[Identity]bool) (bool, error) {
	if pipeline.StaticPause {
		return true, nil
	}
	for _, node := range pipeline.AgentNodes {
		attachment, ok := attached(pipeline, node.Alias)
		if !ok || visiting[attachment.Identity] {
			continue
		}
		child, ok, err := w.read(ctx, attachment.Identity)
		if err != nil {
			return false, err
		}
		if !ok || child.Kind != KindPipeline {
			continue
		}
		visiting[attachment.Identity] = true
		if paused, err := w.pauses(ctx, child, visiting); err != nil || paused {
			return paused, err
		}
	}
	return false, nil
}

func attached(pipeline Version, alias string) (Attachment, bool) {
	for _, attachment := range pipeline.Attachments {
		if attachment.Alias == alias {
			return attachment, true
		}
	}
	return Attachment{}, false
}

func violation(bound Bound, message string) *Violation {
	return &Violation{Bound: bound, message: message}
}

func depthViolation(at string) *Violation {
	return violation(BoundPipelineDepth, fmt.Sprintf(
		"Pipelines are nested more than %d levels deep: %s. Reduce the nesting before saving.",
		MaxPipelineDepth, at))
}

func chain(trail []string, next string) string {
	return strings.Join(append(append([]string(nil), trail...), label(next)), " → ")
}

// maxLabelRunes bounds a name echoed in a refusal.
const maxLabelRunes = 64

// label quotes a stored name for a refusal: printable, bounded, never empty.
func label(name string) string {
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || name == "" {
		return `"unnamed"`
	}
	var builder strings.Builder
	runes := 0
	for _, r := range name {
		if runes == maxLabelRunes {
			builder.WriteString("…")
			break
		}
		if !unicode.IsPrint(r) || r == '"' {
			r = ' '
		}
		builder.WriteRune(r)
		runes++
	}
	return `"` + builder.String() + `"`
}

// ReadPipeline reads the Agent nodes and the static pause of a pipeline
// definition. A definition the save-time admission refuses or that does not
// parse has none: the Worker refuses it on its own.
func ReadPipeline(instructions string) (nodes []AgentNode, staticPause bool) {
	if pipelinelimits.Check(instructions) != nil {
		return nil, false
	}
	var document yaml.Node
	if yaml.NewDecoder(strings.NewReader(instructions)).Decode(&document) != nil || len(document.Content) != 1 {
		return nil, false
	}
	root := aliased(document.Content[0])
	if root.Kind != yaml.MappingNode {
		return nil, false
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		value := aliased(root.Content[i+1])
		switch aliased(root.Content[i]).Value {
		case "interrupt_before", "interrupt_after":
			if value.Kind == yaml.SequenceNode && len(value.Content) > 0 {
				staticPause = true
			}
		case "nodes":
			if value.Kind == yaml.SequenceNode {
				nodes = agentNodes(value)
			}
		}
	}
	return nodes, staticPause
}

func agentNodes(sequence *yaml.Node) []AgentNode {
	var nodes []AgentNode
	for _, entry := range sequence.Content {
		entry = aliased(entry)
		if entry.Kind != yaml.MappingNode {
			continue
		}
		var node AgentNode
		var isAgent bool
		for i := 0; i+1 < len(entry.Content); i += 2 {
			value := aliased(entry.Content[i+1])
			if value.Kind != yaml.ScalarNode {
				continue
			}
			switch aliased(entry.Content[i]).Value {
			case "type":
				isAgent = value.Value == "agent"
			case "id":
				node.ID = value.Value
			case "tool":
				node.Alias = value.Value
			}
		}
		if isAgent && node.Alias != "" {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

func aliased(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		return node.Alias
	}
	return node
}
