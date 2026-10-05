/**
 * `FlowEditor.jsx`'s `nodeTypes`/`edgeTypes` maps (baseline lines 57-83),
 * split into their own file purely to keep `FlowEditor.tsx` itself under
 * the §3.5 400-line file-length budget.
 *
 * REAL PLUMBING GAP THIS SUB-UNIT (A2k) CLOSES, not invented scope: every
 * `versionTools`/`llmSettings`-accepting node component's own doc comment
 * (`AgentNode.tsx`, `CodeNode.tsx`, `HITLNode.tsx`, etc. — units A2e/A2f/
 * A2g) documents the SAME "ambient Formik context -> explicit prop, the
 * CALLER supplies it" redesign, and each says the caller is "the not-yet-
 * built pipeline-editor form, out of this node-renderer sub-unit's scope".
 * `@xyflow/react`'s `nodeTypes` map only ever renders a bare
 * `<Component {...nodeProps} />` with its own `NodeProps` (`id`/`data`/
 * `selected`/…) — there is no channel for a caller to inject extra
 * per-instance props through the map itself. Since this sub-unit owns the
 * ONE place (`FlowEditor.tsx`'s `nodeTypes` wiring) where that map is
 * built, closing over `versionTools`/`llmSettings` in small per-slot
 * wrapper components here is the only layer-legal spot to bridge the gap;
 * `FlowEditor.tsx` gains the two values as its own new props, threaded
 * from whichever future page/form owns them (same "props over store,
 * deferred to page owner" pattern this sub-unit's other new props already
 * use — see `FlowEditor.tsx`'s own doc comment).
 *
 * `[FlowEditorConstants.RUN_STATE_NODE]: FlowEditorNodes.RunStateNode`
 * (baseline `FlowEditor.jsx:75`) is DROPPED, not silently kept: `unit A2f`'s
 * own `RunStateNode.tsx` doc comment states directly "`RunStateNode` is NOT
 * a React-Flow-registered node type — it is a small 'run history' pill
 * rendered by the flow editor's own run-controls chrome
 * (`RunStateNodeGroup`)". Its landed prop shape
 * (`deleteRunNode`/`onStopRun`/`yamlJsonObject` required props) confirms
 * this — it is structurally incompatible with `NodeTypes`'s bare
 * `NodeProps` contract, so the baseline's own map entry was already dead
 * code (no flow node is ever created with `type: 'run_state'`).
 */
import type { ComponentType, ReactNode } from 'react';
import { useMemo } from 'react';

import type { EdgeTypes, NodeProps, NodeTypes } from '@xyflow/react';

import type { AiAssistantLlmSettings } from '../api/aiAssistantPredict';
import { FlowEditorConstants } from '../lib/flow-editor/constants';
import type { FlowNode } from '../lib/flow-editor/reactFlowTypes';
import { ParallelNode } from './nodes/ParallelNode';
import { MapNode } from './nodes/MapNode';
import { SplitOutNode } from './nodes/SplitOutNode';
import { AggregateNode } from './nodes/AggregateNode';
import { AgentNode } from './nodes/AgentNode';
import { CodeNode } from './nodes/CodeNode';
import { CustomEdge } from './nodes/CustomEdge';
import { DecisionNode } from './nodes/DecisionNode';
import { DefaultNode } from './nodes/DefaultNode';
import { EndNode } from './nodes/EndNode';
import { GhostNode } from './nodes/GhostNode';
import { HITLNode } from './nodes/HITLNode';
import { LLMNode } from './nodes/LLMNode';
import { McpNode } from './nodes/McpNode';
import { PrinterNode } from './nodes/PrinterNode';
import { RouterNode } from './nodes/RouterNode';
import { StateModifierNode } from './nodes/StateModifierNode';
import { SubgraphNode } from './nodes/SubgraphNode';
import { ToolkitNode } from './nodes/ToolkitNode';
import { ConditionNode } from './nodes/deprecated/ConditionNode';
import { FunctionNode } from './nodes/deprecated/FunctionNode';
import { LoopNode } from './nodes/deprecated/LoopNode';
import { LoopToolNode } from './nodes/deprecated/LoopToolNode';
import { ToolNode } from './nodes/deprecated/ToolNode';
import type { PipelineToolEntry } from './select/pipelineToolEntry.types';

/** `withVersionTools`/`withLlmSettings`'s shared shape — every wrapper below returns exactly this. */
type FlowNodeComponent = ComponentType<NodeProps<FlowNode>>;

/**
 * `Component` is typed `ComponentType<Record<string, unknown>>` — NOT a
 * `<TProps extends Record<string, unknown>>` generic inferred from each
 * call site. That generic form was tried first and rejected by real `tsc`
 * output: TS infers `TProps` from each concrete node component's OWN
 * props interface (`AgentNodeProps`, `CodeNodeProps`, …), every one of
 * which fails the `extends Record<string, unknown>` constraint (none
 * declare an index signature), so TS falls back to the bare constraint
 * and then rejects passing the real, more specific component in its
 * place ("`MemoExoticComponent<(props: AgentNodeProps) => ReactNode>` is
 * not assignable to `ComponentType<Record<string, unknown>>`" — the exact
 * error, for every single node). A single documented cast at each call
 * site in `buildNodeTypes` (`Component as unknown as ComponentType<Record
 * <string, unknown>>`) — the same "cast once, documented" precedent this
 * whole module already uses for `versionTools`'s own `PipelineToolEntry`/
 * `VersionTool` mismatch — sidesteps the inference problem entirely.
 */
type AnyNodeComponent = ComponentType<Record<string, unknown>>;

/** Wraps a node component so it also receives the current `versionTools` value, closed over from `FlowEditor`'s own props (see module doc comment). */
function withVersionTools(Component: AnyNodeComponent, versionTools: readonly PipelineToolEntry[] | undefined): FlowNodeComponent {
  function VersionToolsNode(props: NodeProps<FlowNode>): ReactNode {
    return (
      <Component
        {...props}
        versionTools={versionTools}
      />
    );
  }
  return VersionToolsNode;
}

/** Same bridge as {@link withVersionTools}, for the node components that instead take `llmSettings`. */
function withLlmSettings(Component: AnyNodeComponent, llmSettings: AiAssistantLlmSettings | null | undefined): FlowNodeComponent {
  function LlmSettingsNode(props: NodeProps<FlowNode>): ReactNode {
    return (
      <Component
        {...props}
        llmSettings={llmSettings}
      />
    );
  }
  return LlmSettingsNode;
}

/** `LLMNode` is the one node component that needs BOTH bridged values at once — a dedicated wrapper instead of composing {@link withVersionTools}/{@link withLlmSettings} (which would double-wrap and obscure the actual prop shape). */
function withVersionToolsAndLlmSettings(Component: AnyNodeComponent, versionTools: readonly PipelineToolEntry[] | undefined, llmSettings: AiAssistantLlmSettings | null | undefined): FlowNodeComponent {
  function VersionToolsAndLlmSettingsNode(props: NodeProps<FlowNode>): ReactNode {
    return (
      <Component
        {...props}
        versionTools={versionTools}
        llmSettings={llmSettings}
      />
    );
  }
  return VersionToolsAndLlmSettingsNode;
}

/**
 * `AgentNode.tsx`'s own established precedent for this exact family of
 * mismatch ("the same runtime array is passed to both, cast once at that
 * one call site") — every node component's props interface is a slightly
 * different, independently-authored subset/superset of `NodeProps` (some
 * extend it, some redeclare `id`/`data`/`selected` by hand with a
 * narrower `data` type; `LLMNode`'s `versionTools` field type is
 * `VersionTool[]`, not `PipelineToolEntry[]`), so a single shared
 * structural type can't describe all of them — this cast is the
 * documented boundary.
 */
function asAnyNodeComponent<TProps extends object>(Component: ComponentType<TProps>): AnyNodeComponent {
  return Component as unknown as AnyNodeComponent;
}

export interface UseFlowEditorNodeTypesArgs {
  readonly versionTools: readonly PipelineToolEntry[] | undefined;
  readonly llmSettings: AiAssistantLlmSettings | null | undefined;
}

export interface FlowEditorNodeTypeMaps {
  readonly nodeTypes: NodeTypes;
  readonly edgeTypes: EdgeTypes;
}

const EDGE_TYPES: EdgeTypes = { custom: CustomEdge };

/** `FlowEditor.jsx:61-83`'s `nodeTypes` map, pure function so `useFlowEditorNodeTypes`'s `useMemo` body is a one-liner and this shape is unit-testable without rendering a hook. */
function buildNodeTypes(versionTools: readonly PipelineToolEntry[] | undefined, llmSettings: AiAssistantLlmSettings | null | undefined): NodeTypes {
  const types = FlowEditorConstants.PipelineNodeTypes;
  return {
    [types.Parallel]: ParallelNode,
    [types.Map]: MapNode,
    [types.SplitOut]: SplitOutNode,
    [types.Aggregate]: AggregateNode,
    [types.Tool]: withVersionTools(asAnyNodeComponent(ToolNode), versionTools),
    [types.Agent]: withVersionTools(asAnyNodeComponent(AgentNode), versionTools),
    [types.Pipeline]: withVersionTools(asAnyNodeComponent(SubgraphNode), versionTools),
    [types.LLM]: withVersionToolsAndLlmSettings(asAnyNodeComponent(LLMNode), versionTools, llmSettings),
    [types.Code]: withLlmSettings(asAnyNodeComponent(CodeNode), llmSettings),
    [types.Function]: withVersionTools(asAnyNodeComponent(FunctionNode), versionTools),
    [types.Condition]: withLlmSettings(asAnyNodeComponent(ConditionNode), llmSettings),
    [types.Decision]: withLlmSettings(asAnyNodeComponent(DecisionNode), llmSettings),
    [types.Loop]: withVersionTools(asAnyNodeComponent(LoopNode), versionTools),
    [types.LoopFromTool]: withVersionTools(asAnyNodeComponent(LoopToolNode), versionTools),
    [types.Router]: withLlmSettings(asAnyNodeComponent(RouterNode), llmSettings),
    [types.StateModifier]: withLlmSettings(asAnyNodeComponent(StateModifierNode), llmSettings),
    [types.Toolkit]: withVersionTools(asAnyNodeComponent(ToolkitNode), versionTools),
    [types.Mcp]: withVersionTools(asAnyNodeComponent(McpNode), versionTools),
    [types.Printer]: withLlmSettings(asAnyNodeComponent(PrinterNode), llmSettings),
    [types.Hitl]: withLlmSettings(asAnyNodeComponent(HITLNode), llmSettings),
    [types.Custom]: withVersionTools(asAnyNodeComponent(DefaultNode), versionTools),
    [types.Default]: withVersionTools(asAnyNodeComponent(DefaultNode), versionTools),
    [types.Ghost]: GhostNode,
    [types.End]: EndNode,
  };
}

/** Memoised `{nodeTypes, edgeTypes}` for `<ReactFlow>` — only rebuilds (and only remounts every node instance) when `versionTools`/`llmSettings` themselves change, not on every `FlowEditor` render. */
export function useFlowEditorNodeTypes(args: UseFlowEditorNodeTypesArgs): FlowEditorNodeTypeMaps {
  const { versionTools, llmSettings } = args;
  const nodeTypes = useMemo(() => buildNodeTypes(versionTools, llmSettings), [versionTools, llmSettings]);
  return { nodeTypes, edgeTypes: EDGE_TYPES };
}
