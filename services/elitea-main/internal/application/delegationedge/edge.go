// Package delegationedge applies the delegation rule of ADR-0044 Amendment 1 to
// the one composition edge the worker never sees: the nested agent a desktop
// runs as a cloud child turn (issue 2214).
//
// The desktop's local agent loop calls a saved agent X it has as a tool by
// starting X as its own cloud turn (startLocalChildTurn). The worker runs that
// turn with X as ITS root, so the edge from the desktop's agent Y to X is
// crossed in Main, at the child start, and nowhere else. This package reads
// the edge's grant there and freezes the result into the child's input, the
// way Main already freezes the child's tool list:
//
//	E(X) = own(X) ∩ (E(Y) ∪ G(Y→X)) ∩ P
//
// with the worker's own rule (libs/rust/agent-runtime delegation.rs
// delegate_edge, delegation/grants.rs narrow_with_grant), mirrored in
// delegationgrant.NarrowWithGrant. P is not applied here: the child's command
// carries the run principal's bound on the principal_bound switch and the
// worker applies it to the child's root, so the intersection happens once.
//
//   - an edge with an approved (or stale) grant narrows the child to
//     own(X) ∩ (E(Y) ∪ G) and runs it; what it holds beyond that is
//     narrowed out and shown as a `delegation_narrowed` notice;
//   - an edge with none refuses a child that holds something its caller does
//     not (enforce mode; a `delegation_refused` notice, reason `no_grant`),
//     and cuts a child that holds no more than its caller to the caller's
//     tool selections, as the worker does. Report mode records and runs.
//
// It acts only while ELITEA_DELEGATION_GRANTS_ENFORCED is on (off by default
// until the migration of existing compositions, story S6, has created their
// grants), like the worker's side. With it off the child start is unchanged.
package delegationedge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/delegationnotice"
	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// SettingsReader reads the platform `delegation` section.
type SettingsReader interface {
	Get(ctx context.Context) (platformconfig.DelegationSettings, error)
}

// Callees reads what a version holds now.
type Callees interface {
	Snapshot(ctx context.Context, projectID, applicationID, versionID int64) (dg.Callee, error)
}

// Grants reads the approved and stale grants whose caller is one of callerIDs.
type Grants interface {
	ConsumableFromCallers(ctx context.Context, projectID int64, callerIDs []int64) ([]dg.Grant, error)
}

// Edge is the desktop agent Y and the nested agent X it starts as a child.
type Edge struct {
	ProjectID                            int64
	CallerApplicationID, CallerVersionID int64
	CalleeApplicationID, CalleeVersionID int64
	// NoticeID is the id the notice carries (the start's idempotency key); the
	// desktop replaces it with the call it answers.
	NoticeID string
}

// RefusedError is a child start the edge's rule refused. The notice is what the
// person is shown.
type RefusedError struct {
	Notice delegationnotice.Notice
}

func (e *RefusedError) Error() string {
	return "delegation refused: the nested agent holds something its caller does not, and the edge has no approved grant"
}

// Outcome is what the rule did to one child start: a notice when the child was
// narrowed. A refusal is the RefusedError the hook returns.
type Outcome struct {
	Notice *delegationnotice.Notice
}

// Hook rewrites the child's frozen version before it is admitted.
type Hook func(ctx context.Context, versionDetails json.RawMessage) (json.RawMessage, error)

// Evaluator evaluates edges. A nil *Evaluator evaluates none.
type Evaluator struct {
	settings SettingsReader
	callees  Callees
	grants   Grants
}

// New builds the evaluator.
func New(settings SettingsReader, callees Callees, grants Grants) (*Evaluator, error) {
	if settings == nil || callees == nil || grants == nil {
		return nil, errors.New("delegation edge: the settings, callee and grant readers are required")
	}
	return &Evaluator{settings: settings, callees: callees, grants: grants}, nil
}

// Hook answers the rewrite of the child's frozen version for the edge, and
// records on out what it did. A nil evaluator answers nil: the start is
// unchanged. So does an edge from an agent to itself: a cloud agent of a
// desktop thread is relayed as the turn's own agent (ADR-0050 decision 7f),
// and that is no delegation.
func (e *Evaluator) Hook(edge Edge, out *Outcome) Hook {
	if e == nil || edge.CallerApplicationID == edge.CalleeApplicationID {
		return nil
	}
	return func(ctx context.Context, versionDetails json.RawMessage) (json.RawMessage, error) {
		return e.apply(ctx, edge, out, versionDetails)
	}
}

func (e *Evaluator) apply(ctx context.Context, edge Edge, out *Outcome, versionDetails json.RawMessage) (json.RawMessage, error) {
	callee, err := e.callees.Snapshot(ctx, edge.ProjectID, edge.CalleeApplicationID, edge.CalleeVersionID)
	if err != nil {
		return nil, fmt.Errorf("delegation edge: read the nested agent: %w", err)
	}
	caller, err := e.callees.Snapshot(ctx, edge.ProjectID, edge.CallerApplicationID, edge.CallerVersionID)
	if err != nil {
		return nil, fmt.Errorf("delegation edge: read the calling agent: %w", err)
	}
	settings, err := e.settings.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("delegation edge: read the delegation setting: %w", err)
	}
	// A deployment that never saved the section enforces, as the worker's
	// compiled-in default does.
	enforce := !settings.Stored || settings.Mode != platformconfig.DelegationReport
	grant, err := e.edgeGrant(ctx, edge)
	if err != nil {
		return nil, err
	}
	labels := toolkitLabels(versionDetails)
	notice := delegationnotice.Notice{
		ID: edge.NoticeID,
		Edge: delegationnotice.Edge{
			CallerID: edge.CallerApplicationID, CallerName: caller.Name,
			CalleeID: edge.CalleeApplicationID, CalleeName: callee.Name,
			Kind: string(dg.EdgeAgentTool),
		},
		Missing: []delegationnotice.Item{},
	}
	var effective dg.GrantSet
	switch {
	case grant != nil:
		var narrowed []dg.Item
		effective, narrowed = dg.NarrowWithGrant(caller.Set, grant.GrantSet, callee.Set, labels)
		if len(narrowed) == 0 {
			return versionDetails, nil
		}
		notice.Kind, notice.Reason, notice.GrantID = "narrowed", "stale", grant.ID
		notice.Missing, notice.MissingTotal = noticeItems(narrowed), len(narrowed)
	default:
		missing, refused := dg.RefuseWithoutGrant(caller.Set, callee.Set, labels)
		if refused && !enforce {
			slog.InfoContext(ctx, "delegation edge: report mode, the nested agent holds more than its caller and the edge has no grant",
				"project_id", edge.ProjectID, "caller", edge.CallerApplicationID, "callee", edge.CalleeApplicationID)
			return versionDetails, nil
		}
		if refused {
			notice.Kind, notice.Reason, notice.Code = "refused", "no_grant", "DELEGATION_GRANT_MISSING"
			notice.Missing, notice.MissingTotal = noticeItems([]dg.Item{missing}), 1
			return nil, &RefusedError{Notice: notice}
		}
		// Holds no more than its caller: cut to the caller's selections, with
		// nothing to show.
		effective = dg.NarrowToCaller(caller.Set, callee.Set)
	}
	narrowedVersion, err := applyEffective(versionDetails, callee.Set, effective)
	if err != nil {
		return nil, err
	}
	if notice.Kind == "narrowed" {
		out.Notice = &notice
	}
	return narrowedVersion, nil
}

// edgeGrant is the approved or stale grant of the edge, whatever its kind, read
// in the order the worker reads them: an agent attached as a tool first.
func (e *Evaluator) edgeGrant(ctx context.Context, edge Edge) (*dg.Grant, error) {
	grants, err := e.grants.ConsumableFromCallers(ctx, edge.ProjectID, []int64{edge.CallerApplicationID})
	if err != nil {
		return nil, fmt.Errorf("delegation edge: read the edge's grant: %w", err)
	}
	for _, kind := range []dg.EdgeKind{dg.EdgeAgentTool, dg.EdgePipelineChild, dg.EdgeSubAgent, dg.EdgePipelineNode} {
		for i := range grants {
			if grants[i].Edge.CalleeEntityID == edge.CalleeApplicationID && grants[i].Edge.Kind == kind {
				return &grants[i], nil
			}
		}
	}
	return nil, nil
}

func noticeItems(items []dg.Item) []delegationnotice.Item {
	out := make([]delegationnotice.Item, 0, len(items))
	for _, item := range items {
		out = append(out, delegationnotice.Item{Kind: item.Kind, Name: item.Name, Toolkit: item.Toolkit})
	}
	return out
}

// toolkitLabels names the toolkits of a frozen version by their key.
func toolkitLabels(versionDetails json.RawMessage) dg.EdgeLabels {
	labels := dg.EdgeLabels{}
	tools, _ := decodeTools(versionDetails)
	for _, tool := range tools {
		if key, ok := toolKey(tool); ok {
			if name, _ := tool["toolkit_name"].(string); name != "" {
				labels[key] = name
			}
		}
	}
	return labels
}

func decodeVersion(versionDetails json.RawMessage) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(versionDetails))
	decoder.UseNumber()
	var version map[string]any
	if err := decoder.Decode(&version); err != nil {
		return nil, fmt.Errorf("delegation edge: the frozen version is not an object: %w", err)
	}
	return version, nil
}

func decodeTools(versionDetails json.RawMessage) ([]map[string]any, error) {
	version, err := decodeVersion(versionDetails)
	if err != nil {
		return nil, err
	}
	var tools []map[string]any
	for _, entry := range asList(version["tools"]) {
		if tool, ok := entry.(map[string]any); ok {
			tools = append(tools, tool)
		}
	}
	return tools, nil
}

func asList(value any) []any {
	list, _ := value.([]any)
	return list
}

// toolKey is the grant set key of a frozen toolkit entry: `toolkit:<id>` for a
// row, `builtin:<type>` for one with none; false for a nested application,
// which is an edge of its own.
func toolKey(tool map[string]any) (string, bool) {
	toolType, _ := tool["type"].(string)
	if toolType == "" || toolType == "application" {
		return "", false
	}
	if id, ok := tool["id"].(json.Number); ok {
		if n, err := strconv.ParseInt(id.String(), 10, 64); err == nil && n > 0 {
			return "toolkit:" + strconv.FormatInt(n, 10), true
		}
	}
	return "builtin:" + toolType, true
}

// applyEffective writes the effective selections into the child's frozen
// tools, as the worker does for a delegated callee: a toolkit narrowed to some
// tools gets `selected_tools`, one that keeps every tool but more exclusions
// gets `excluded_tools`, one left with none is removed. A toolkit the callee's
// own set (`own`, read from its toolkit rows) does not list is left as frozen:
// it is not this rule's to narrow.
func applyEffective(versionDetails json.RawMessage, own, effective dg.GrantSet) (json.RawMessage, error) {
	version, err := decodeVersion(versionDetails)
	if err != nil {
		return nil, err
	}
	considered := map[string]bool{}
	for _, toolkit := range own.Toolkits {
		considered[toolkit.Key] = true
	}
	granted := map[string]dg.ToolkitGrant{}
	for _, toolkit := range effective.Toolkits {
		granted[toolkit.Key] = toolkit
	}
	var kept []any
	for _, entry := range asList(version["tools"]) {
		tool, ok := entry.(map[string]any)
		if !ok {
			kept = append(kept, entry)
			continue
		}
		key, isToolkit := toolKey(tool)
		if !isToolkit || !considered[key] {
			kept = append(kept, entry)
			continue
		}
		grant, held := granted[key]
		if !held {
			continue
		}
		if settings, ok := tool["settings"].(map[string]any); ok {
			switch {
			case grant.Tools.Mode == dg.ToolsOnly:
				settings["selected_tools"] = stringList(grant.Tools.Names)
			case len(grant.Tools.Names) > 0:
				settings["excluded_tools"] = stringList(grant.Tools.Names)
			}
		}
		kept = append(kept, tool)
	}
	if kept == nil {
		kept = []any{}
	}
	version["tools"] = kept
	out, err := json.Marshal(version)
	if err != nil {
		return nil, fmt.Errorf("delegation edge: write the narrowed version: %w", err)
	}
	return out, nil
}

func stringList(names []string) []any {
	out := make([]any, 0, len(names))
	for _, name := range names {
		out = append(out, name)
	}
	return out
}
