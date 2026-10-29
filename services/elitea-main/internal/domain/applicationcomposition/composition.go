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
	// MaxCheckpointPaths is the most Agent-node paths one saved pipeline
	// composition holds (a pipeline root or tool and the saved pipelines its
	// Agent nodes run).
	MaxCheckpointPaths = 128
	// MaxVersionBytes is the Worker's load budget for one composition. Main
	// cannot measure the frozen documents the Worker loads, so it is not
	// checked here: an attachment past it is left out and named, and a root
	// that cannot load its own composition ends with SizeRuntimeMessage.
	MaxVersionBytes = 4 * 1024 * 1024
)

// Code is the public refusal code; the Worker's run-time refusal maps to it too.
const Code = "APPLICATION_COMPOSITION_INVALID"

// RuntimeMessage is the registered, data-free text of a run refused for its
// stored composition (application-composition-limits-v1.json runtime_message):
// the Worker sends it, and Main's start refusal (ErrAtStart) answers it.
const RuntimeMessage = "This agent or pipeline cannot run: its attachments form a circular reference, nest deeper than allowed, or include a pipeline that pauses where the pause cannot be continued. Review its attachments and change the one that breaks the rule."

// SizeRuntimeMessage is the registered, data-free text of a run whose own
// composition exceeds MaxVersionBytes (size_runtime_message).
const SizeRuntimeMessage = "This agent or pipeline cannot run: its attached agents and pipelines are too large to load together. Remove attachments or shorten their instructions."

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
	BoundCheckpoints   Bound = "checkpoint_paths"
	// FU-249: a saved pipeline with a Parallel node runs only as the started root.
	BoundNestedParallel Bound = "parallel_in_child_pipeline"
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
	// Branch: a fixed Parallel node runs it as one of its branches.
	Branch bool
}

// Version is what the walk reads of one saved version.
type Version struct {
	Kind        Kind
	Name        string
	Attachments []Attachment
	// Pipeline only, from ReadPipeline.
	AgentNodes  []AgentNode
	StaticPause bool
	// ParallelNode is the first Parallel node's id, empty when there is none.
	ParallelNode string
	// LLMBranches is how many LLM nodes a Parallel runs as branches: each
	// runs in its own checkpoint family (PM-4).
	LLMBranches int
}

// Source reads one saved version. ok is false for a version that does not
// exist (or cannot be read as a composition); the walk does not follow it.
type Source interface {
	Version(ctx context.Context, id Identity) (version Version, ok bool, err error)
}

// Prefetcher is a Source that reads many versions in one round trip. The walk
// prefetches the versions it can reach level by level before it walks them, so
// a check costs a few batched reads instead of one read per version.
type Prefetcher interface {
	Prefetch(ctx context.Context, ids []Identity) error
}

// Ancestry is a Source that answers, in one round trip, the versions that
// attach any of ids as a tool.
type Ancestry interface {
	Parents(ctx context.Context, ids []Identity) ([]Identity, error)
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
	var atStart *apierr.APIError
	if errors.As(err, &atStart) && atStart.Code == Code {
		return atStart
	}
	var violation *Violation
	if !errors.As(err, &violation) {
		return nil
	}
	return &apierr.APIError{Status: http.StatusBadRequest, Code: Code, Message: violation.message}
}

// AtStart is the start refusal for a refused composition err. A Parallel in a
// child pipeline (FU-249) is named, as its save refusal is; every other bound
// answers the registered RuntimeMessage the Worker's assembly refusal carries.
func AtStart(err error) error {
	var violation *Violation
	if errors.As(err, &violation) && violation.Bound == BoundNestedParallel {
		return &apierr.APIError{Status: http.StatusUnprocessableEntity, Code: Code, Message: violation.message}
	}
	return ErrAtStart
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
	_, err := check(ctx, source, root, *version, nil)
	return err
}

// CheckWithAncestors is Check for a version that is about to change, followed
// by the same check of every saved version that attaches it, directly or
// through others (PLAN §8.8): a change below can push an ancestor past a bound
// (a fourth agent level, a cycle) although the changed version alone fits. The
// ancestors read root as override. Up to MaxCycleVersions ancestors are found,
// level by level when source is an Ancestry; a source that is not checks root
// alone.
func CheckWithAncestors(ctx context.Context, source Source, root Identity, override *Version) error {
	if err := Check(ctx, source, root, override); err != nil {
		return err
	}
	ancestry, ok := source.(Ancestry)
	if !ok {
		return nil
	}
	ancestors, err := ancestorsOf(ctx, ancestry, root)
	if err != nil {
		return err
	}
	if len(ancestors) == 0 {
		return nil
	}
	if batch, ok := source.(Prefetcher); ok {
		if err := batch.Prefetch(ctx, ancestors); err != nil {
			return err
		}
	}
	view := source
	if override != nil {
		view = overridden{Source: source, id: root, version: *override}
	}
	for _, ancestor := range ancestors {
		if err := Check(ctx, view, ancestor, nil); err != nil {
			return err
		}
	}
	return nil
}

// ancestorsOf answers the versions that attach root, transitively, nearest first.
func ancestorsOf(ctx context.Context, ancestry Ancestry, root Identity) ([]Identity, error) {
	seen := map[Identity]bool{root: true}
	var ancestors []Identity
	for frontier := []Identity{root}; len(frontier) > 0 && len(ancestors) < MaxCycleVersions; {
		parents, err := ancestry.Parents(ctx, frontier)
		if err != nil {
			return nil, err
		}
		frontier = nil
		for _, parent := range parents {
			if seen[parent] || len(ancestors) == MaxCycleVersions {
				continue
			}
			seen[parent] = true
			ancestors = append(ancestors, parent)
			frontier = append(frontier, parent)
		}
	}
	return ancestors, nil
}

// overridden reads one version as it is about to be stored.
type overridden struct {
	Source
	id      Identity
	version Version
}

func (o overridden) Version(ctx context.Context, id Identity) (Version, bool, error) {
	if id == o.id {
		version := o.version
		version.Attachments = append([]Attachment(nil), o.version.Attachments...)
		version.AgentNodes = append([]AgentNode(nil), o.version.AgentNodes...)
		return version, true, nil
	}
	return o.Source.Version(ctx, id)
}

func (o overridden) Prefetch(ctx context.Context, ids []Identity) error {
	if batch, ok := o.Source.(Prefetcher); ok {
		return batch.Prefetch(ctx, ids)
	}
	return nil
}

// Fit is whether one participant can be offered to a conversation's chat model.
// Tiers is its own agent subtree tiers (pipelines consume none) when it fits,
// and MaxAgentTiers, the nesting limit, when it does not, so a reader's
// `1 + tiers > MaxAgentTiers` gate flags exactly the participants an ad-hoc
// turn does not offer (PLAN §8.8).
type Fit struct {
	Tiers int
	Fits  bool
}

// ChatToolFits answers, in order, which participants an ad-hoc turn can offer
// its chat model. The Worker builds every offered participant as an attachment
// of one agent root, one level below where each runs when selected directly,
// and they share that root's hop budget. A participant is offered when it fits
// together with the ones offered before it; one that does not is not offered
// and does not consume the budget. A version that cannot be read fits and is
// left to the turn's own resolution.
func ChatToolFits(ctx context.Context, source Source, ids []Identity) ([]Fit, error) {
	fits := make([]Fit, len(ids))
	chat := Version{Kind: KindAgent, Name: "chat"}
	for _, id := range ids {
		chat.Attachments = append(chat.Attachments, Attachment{Identity: id})
	}
	w := walker{source: source, cache: map[Identity]Version{}, maxTier: 1}
	if err := w.prefetch(ctx, []Version{chat}); err != nil {
		return nil, err
	}
	root := path{ancestors: []Identity{{}}, trail: []string{label(chat.Name)}, tier: 1}
	seen := map[Identity]bool{}
	for i, id := range ids {
		participant, ok, err := w.read(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			fits[i] = Fit{Fits: true}
			continue
		}
		hops, maxTier, wasSeen := w.hops, w.maxTier, seen[id]
		w.maxTier = 1
		alone := Version{Attachments: []Attachment{{Identity: id}}}
		err = w.cycles(ctx, alone, map[Identity]int{{}: visiting}, root.trail)
		if err == nil {
			err = w.attach(ctx, chat, root, Attachment{Identity: id}, participant, seen)
		}
		var refused *Violation
		switch {
		case errors.As(err, &refused):
			w.hops, seen[id] = hops, wasSeen
			fits[i] = Fit{Tiers: MaxAgentTiers}
		case err != nil:
			return nil, err
		default:
			fits[i] = Fit{Tiers: w.maxTier - 1, Fits: true}
		}
		w.maxTier = max(w.maxTier, maxTier)
	}
	return fits, nil
}

// check walks version as root and answers the deepest agent tier it reached;
// known holds versions already read.
func check(ctx context.Context, source Source, root Identity, version Version, known map[Identity]Version) (int, error) {
	cache := map[Identity]Version{root: version}
	for id, read := range known {
		cache[id] = read
	}
	w := walker{source: source, cache: cache, maxTier: 1}
	if err := w.prefetch(ctx, []Version{version}); err != nil {
		return 0, err
	}
	// A cycle through ANY attachment is refused (PLAN §8.8), whether or not a
	// pipeline's Agent node runs it: an agent reaching itself is an authoring
	// error even where today's run would not follow the edge.
	if err := w.cycles(ctx, version, map[Identity]int{root: visiting}, []string{label(version.Name)}); err != nil {
		return 0, err
	}
	start := path{ancestors: []Identity{root}, trail: []string{label(version.Name)}, tier: 1}
	if version.Kind == KindPipeline {
		start.depth = 1
		start.graphRoot = true
		return w.maxTier, w.walkPipeline(ctx, version, start, 0)
	}
	return w.maxTier, w.walkAgent(ctx, version, start)
}

// path is one branch's immutable ancestry; hops are shared by the walker.
type path struct {
	ancestors []Identity
	trail     []string
	tier      int
	depth     int
	scopes    int
	graphRoot bool
	// underAgent: a saved Agent lies on the path, so a pipeline below it runs
	// as that Agent's tool, not as a subgraph of the root graph.
	underAgent bool
	// underBranch: the path runs inside a fixed Parallel branch.
	underBranch bool
}

func (p path) with(id Identity, name string) path {
	next := p
	next.ancestors = append(append([]Identity(nil), p.ancestors...), id)
	next.trail = append(append([]string(nil), p.trail...), label(name))
	return next
}

type walker struct {
	source  Source
	cache   map[Identity]Version
	hops    int
	maxTier int
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

// Cycle search states; a version absent from the map is unvisited.
const (
	visiting = 1
	visited  = 2
)

// maxCycleVersions bounds the versions the cycle search reads in one check
// (MaxCycleVersions exported for the read-budget test).
// Past it the search stops (it does not refuse): the bound walk's hop budget
// still applies to everything a run would build.
const maxCycleVersions = MaxCycleVersions

// MaxCycleVersions is the most versions one check reads.
const MaxCycleVersions = 128

// cycles searches every attachment depth-first; a version met again while it
// is still on the path closes a cycle.
func (w *walker) cycles(ctx context.Context, version Version, state map[Identity]int, trail []string) error {
	for _, attachment := range version.Attachments {
		child, ok, err := w.read(ctx, attachment.Identity)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		switch state[attachment.Identity] {
		case visiting:
			return violation(BoundCycle, fmt.Sprintf(
				"These attachments would create a circular agent reference: %s. Remove the circular dependency first.",
				chain(trail, child.Name)))
		case visited:
			continue
		}
		if len(state) >= maxCycleVersions {
			return nil
		}
		state[attachment.Identity] = visiting
		if err := w.cycles(ctx, child, state, append(append([]string(nil), trail...), label(child.Name))); err != nil {
			return err
		}
		state[attachment.Identity] = visited
	}
	return nil
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
	next.underAgent = true
	w.maxTier = max(w.maxTier, next.tier)
	return next, err
}

func (w *walker) enterPipeline(p path, id Identity, child Version) (path, error) {
	if p.depth > MaxPipelineDepth {
		return path{}, depthViolation(chain(p.trail, child.Name))
	}
	// FU-249: a Parallel runs in the started pipeline and in a saved pipeline it
	// reaches through Agent nodes only (a subgraph of the root graph). As an
	// agent's tool, or inside another Parallel's branch, it cannot run.
	if child.ParallelNode != "" && (!p.graphRoot || p.underAgent || p.underBranch) {
		return path{}, violation(BoundNestedParallel, fmt.Sprintf(
			"Pipeline %s has Parallel node %s, so it cannot run as a tool of an agent or inside a Parallel branch (%s). Run pipeline %s from a pipeline Agent node outside any Parallel branch, start it directly, or remove the attachment before saving.",
			label(child.Name), label(child.ParallelNode), chain(p.trail, child.Name), label(child.Name)))
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
		if err := w.attach(ctx, agent, p, attachment, child, seen); err != nil {
			return err
		}
	}
	return nil
}

// attach enters one attachment of agent at p and walks it.
func (w *walker) attach(ctx context.Context, agent Version, p path, attachment Attachment, child Version, seen map[Identity]bool) error {
	if child.Kind == KindAgent {
		// The Worker builds each attached agent once (application_references).
		if seen[attachment.Identity] {
			return nil
		}
		seen[attachment.Identity] = true
		next, err := w.enterAgent(p, attachment.Identity, child)
		if err != nil {
			return err
		}
		return w.walkAgent(ctx, child, next)
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
	return w.walkPipeline(ctx, child, next, 0)
}

// walkPipeline follows a pipeline's Agent nodes, as a graph's assembly binds them.
// composition is the saved-pipeline nesting below the nearest pipeline root or tool.
func (w *walker) walkPipeline(ctx context.Context, pipeline Version, p path, composition int) error {
	if composition == MaxPipelineDepth && len(pipeline.AgentNodes) > 0 {
		return depthViolation(strings.Join(append(append([]string(nil), p.trail...),
			"Agent node "+label(pipeline.AgentNodes[0].ID)), " → "))
	}
	// PM-2: an inline LLM Parallel branch needs a checkpoint path one level
	// below its pipeline, so at the deepest level it is refused as an Agent
	// node is (the Worker's collect_paths refuses the same).
	if composition == MaxPipelineDepth && pipeline.LLMBranches > 0 {
		return depthViolation(strings.Join(append(append([]string(nil), p.trail...),
			"Parallel node "+label(pipeline.ParallelNode)), " → "))
	}
	if composition == 0 {
		paths := 0
		if err := w.checkpointPaths(ctx, pipeline, 0, &paths, map[Identity]bool{}); err != nil {
			return err
		}
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
		at := p
		if node.Branch {
			at.underBranch = true
		}
		if child.Kind == KindPipeline {
			next, err := w.enterPipeline(at, attachment.Identity, child)
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
		scoped := at
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

// checkpointPaths counts the checkpoint paths of a saved pipeline composition
// as the Worker's collect_paths does: every Agent node, and the nodes of the
// saved pipeline it runs, below the pipeline root or tool, then every inline
// LLM Parallel branch at that level (PM-4). Depth past the bound is
// walkPipeline's refusal; visiting guards a cycle (refused by the cycle check).
func (w *walker) checkpointPaths(ctx context.Context, pipeline Version, depth int, paths *int, visiting map[Identity]bool) error {
	if depth >= MaxPipelineDepth {
		return nil
	}
	for _, node := range pipeline.AgentNodes {
		if *paths == MaxCheckpointPaths {
			return violation(BoundCheckpoints, fmt.Sprintf(
				"Pipeline %s runs more than %d Agent nodes and LLM Parallel branches across the saved pipelines it runs. Reduce them before saving.",
				label(pipeline.Name), MaxCheckpointPaths))
		}
		*paths++
		attachment, ok := attached(pipeline, node.Alias)
		if !ok || visiting[attachment.Identity] {
			continue
		}
		child, ok, err := w.read(ctx, attachment.Identity)
		if err != nil {
			return err
		}
		if !ok || child.Kind != KindPipeline {
			continue
		}
		visiting[attachment.Identity] = true
		err = w.checkpointPaths(ctx, child, depth+1, paths, visiting)
		delete(visiting, attachment.Identity)
		if err != nil {
			return err
		}
	}
	for range pipeline.LLMBranches {
		if *paths == MaxCheckpointPaths {
			return violation(BoundCheckpoints, fmt.Sprintf(
				"Pipeline %s runs more than %d Agent nodes and LLM Parallel branches across the saved pipelines it runs. Reduce them before saving.",
				label(pipeline.Name), MaxCheckpointPaths))
		}
		*paths++
	}
	return nil
}

// prefetch reads, level by level, the versions reachable from roots through
// any attachment, so the walks that follow read from the cache. It stops after
// MaxCycleVersions versions; anything past that is read on demand.
func (w *walker) prefetch(ctx context.Context, roots []Version) error {
	batch, ok := w.source.(Prefetcher)
	if !ok {
		return nil
	}
	seen := map[Identity]bool{}
	var frontier []Identity
	next := func(versions []Version) {
		for _, version := range versions {
			for _, attachment := range version.Attachments {
				if !seen[attachment.Identity] && len(seen) < MaxCycleVersions {
					seen[attachment.Identity] = true
					frontier = append(frontier, attachment.Identity)
				}
			}
		}
	}
	next(roots)
	for len(frontier) > 0 {
		level := frontier
		frontier = nil
		if err := batch.Prefetch(ctx, level); err != nil {
			return err
		}
		read := make([]Version, 0, len(level))
		for _, id := range level {
			version, ok, err := w.read(ctx, id)
			if err != nil {
				return err
			}
			if ok {
				read = append(read, version)
			}
		}
		next(read)
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

// ReadPipeline reads the Agent nodes, the static pause, the first Parallel
// node and the number of inline LLM Parallel branches of a pipeline
// definition. A definition the save-time admission refuses or that does not
// parse has none: the Worker refuses it on its own.
func ReadPipeline(instructions string) (nodes []AgentNode, staticPause bool, parallelNode string, llmBranches int) {
	if pipelinelimits.Check(instructions) != nil {
		return nil, false, "", 0
	}
	var document yaml.Node
	if yaml.NewDecoder(strings.NewReader(instructions)).Decode(&document) != nil || len(document.Content) != 1 {
		return nil, false, "", 0
	}
	root := aliased(document.Content[0])
	if root.Kind != yaml.MappingNode {
		return nil, false, "", 0
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
				nodes, parallelNode, llmBranches = agentNodes(value)
			}
		}
	}
	return nodes, staticPause, parallelNode, llmBranches
}

func agentNodes(sequence *yaml.Node) (nodes []AgentNode, parallelNode string, llmBranches int) {
	branches := map[string]bool{}
	var llmNodes []string
	defer func() {
		for i := range nodes {
			nodes[i].Branch = branches[nodes[i].ID]
		}
		for _, id := range llmNodes {
			if branches[id] {
				llmBranches++
			}
		}
	}()
	for _, entry := range sequence.Content {
		entry = aliased(entry)
		if entry.Kind != yaml.MappingNode {
			continue
		}
		var node AgentNode
		var isAgent, isParallel, isLLM bool
		for i := 0; i+1 < len(entry.Content); i += 2 {
			value := aliased(entry.Content[i+1])
			if aliased(entry.Content[i]).Value == "branches" && value.Kind == yaml.SequenceNode {
				for _, node := range branchNodes(value) {
					branches[node] = true
				}
				continue
			}
			if value.Kind != yaml.ScalarNode {
				continue
			}
			switch aliased(entry.Content[i]).Value {
			case "type":
				isAgent = value.Value == "agent"
				isParallel = value.Value == "parallel"
				isLLM = value.Value == "llm"
			case "id":
				node.ID = value.Value
			case "tool":
				node.Alias = value.Value
			}
		}
		if isAgent && node.Alias != "" {
			nodes = append(nodes, node)
		}
		if isParallel && parallelNode == "" && node.ID != "" {
			parallelNode = node.ID
		}
		if isLLM && node.ID != "" {
			llmNodes = append(llmNodes, node.ID)
		}
	}
	return nodes, parallelNode, llmBranches
}

// branchNodes answers the node ids a Parallel's `branches` list runs.
func branchNodes(sequence *yaml.Node) []string {
	var ids []string
	for _, branch := range sequence.Content {
		branch = aliased(branch)
		if branch.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(branch.Content); i += 2 {
			value := aliased(branch.Content[i+1])
			if aliased(branch.Content[i]).Value == "node" && value.Kind == yaml.ScalarNode {
				ids = append(ids, value.Value)
			}
		}
	}
	return ids
}

func aliased(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		return node.Alias
	}
	return node
}
