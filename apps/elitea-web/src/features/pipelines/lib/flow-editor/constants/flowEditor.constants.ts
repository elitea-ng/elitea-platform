/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/lib/
 * constants/flowEditor.constants.js` (302 lines, unit A2c). Pure data — no
 * behavioural changes from the baseline. `NodeHeightMap` keeps the
 * baseline's untyped-object shape rather than a discriminated union,
 * matching the baseline's own design: the flow editor's consumers read
 * these defensively by node type, never exhaustively.
 *
 * The baseline's `InitialNodeData` (and the `create*NodeData` factories
 * behind it) now lives in `./nodeDefaults.constants.ts` — the per-node-type
 * runtime-contract comments added there pushed this file past the §3.5
 * 400-line budget. Import it from that module directly; this one does not
 * re-export it (that would be an initialization cycle).
 */

/** Suffix appended to a node id to derive its synthetic legacy-condition sub-node id. */
export const CONDITION_NODE_ID_SUFFIX = '~~~ConditionNode';
/** Suffix appended to a node id to derive its synthetic legacy-decision sub-node id. */
export const DECISION_NODE_ID_SUFFIX = '~~~DecisionNode';
export const ROUTER_HANDLE_ID_SUFFIX = 'routerNode';
export const HITL_HANDLE_ID_SUFFIX = 'hitlNode';
/** React Flow's own auto-generated edge id prefix (`@xyflow/react`), reused for synthetic edge ids. */
export const EDGE_PREFIX = 'xy-edge__';
export const DEFAULT_OUTPUT = 'default_output';
export const PIPELINE_STATE = 'state';
export const STATE_MESSAGES = 'messages';
export const STATE_INPUT = 'input';
export const RUN_STATE_NODE = 'run_state';

export const StateVariableTypes = {
  String: 'str',
  Number: 'number',
  List: 'list',
  Json: 'dict',
} as const;

export type StateVariableType = (typeof StateVariableTypes)[keyof typeof StateVariableTypes];

export const DefaultState = {
  [STATE_INPUT]: { type: StateVariableTypes.String },
  [STATE_MESSAGES]: { type: StateVariableTypes.List },
} as const;

export const STATE_INPUT_ATTACHMENTS = 'input_attachments';
export const StateDefaultProps = [STATE_INPUT, STATE_MESSAGES] as const;
export const StateManagedProps = [STATE_INPUT_ATTACHMENTS] as const;

/** A state-variable type value seen only in pre-migration YAML — treated as an alias of `Number`. */
export const LegacyIntType = 'int';

export const PipelineStatus = {
  InProgress: 'In progress',
  Completed: 'Completed',
  Interrupt: 'Interrupt',
  Error: 'Error',
  Stopped: 'Stopped',
} as const;

export type PipelineStatusValue = (typeof PipelineStatus)[keyof typeof PipelineStatus];

export const PipelineNodeTypes = {
  Tool: 'tool',
  Agent: 'agent',
  Pipeline: 'pipeline',
  Function: 'function',
  LLM: 'llm',
  Decision: 'decision',
  Condition: 'condition',
  Loop: 'loop',
  LoopFromTool: 'loop_from_tool',
  Router: 'router',
  StateModifier: 'state_modifier',
  Toolkit: 'toolkit',
  Mcp: 'mcp',
  Code: 'code',
  Parallel: 'parallel',
  Map: 'map',
  SplitOut: 'split_out',
  Aggregate: 'aggregate',
  Printer: 'printer',
  Hitl: 'hitl',
  Custom: 'custom',
  Ghost: 'ghost',
  End: 'END',
  Default: 'defaultType',
} as const;

export type PipelineNodeType = (typeof PipelineNodeTypes)[keyof typeof PipelineNodeTypes];

export const PipelineNodeDisplayNames: Readonly<Record<PipelineNodeType, string>> = {
  [PipelineNodeTypes.Tool]: 'Tool',
  [PipelineNodeTypes.Agent]: 'Agent',
  [PipelineNodeTypes.Pipeline]: 'Pipeline',
  [PipelineNodeTypes.Function]: 'Function',
  [PipelineNodeTypes.LLM]: 'LLM',
  [PipelineNodeTypes.Decision]: 'Decision',
  [PipelineNodeTypes.Condition]: 'Condition',
  [PipelineNodeTypes.Loop]: 'Loop',
  [PipelineNodeTypes.LoopFromTool]: 'Loop from tool',
  [PipelineNodeTypes.Router]: 'Router',
  [PipelineNodeTypes.StateModifier]: 'State modifier',
  [PipelineNodeTypes.Toolkit]: 'Toolkit',
  [PipelineNodeTypes.Mcp]: 'MCP',
  [PipelineNodeTypes.Code]: 'Code',
  [PipelineNodeTypes.Parallel]: 'Parallel',
  [PipelineNodeTypes.Map]: 'Map',
  [PipelineNodeTypes.SplitOut]: 'SplitOut',
  [PipelineNodeTypes.Aggregate]: 'Aggregate',
  [PipelineNodeTypes.Printer]: 'Printer',
  [PipelineNodeTypes.Hitl]: 'Human-in-the-loop',
  [PipelineNodeTypes.Custom]: 'Custom',
  [PipelineNodeTypes.Ghost]: 'Ghost',
  [PipelineNodeTypes.End]: 'End',
  [PipelineNodeTypes.Default]: 'Default',
};

export const NodeHeightMap: Readonly<Record<string, number>> = {
  [PipelineNodeTypes.Condition]: 450,
  [PipelineNodeTypes.Decision]: 450,
  [PipelineNodeTypes.LLM]: 460,
  [PipelineNodeTypes.Tool]: 340,
  [PipelineNodeTypes.Function]: 460,
  [PipelineNodeTypes.Loop]: 460,
  [PipelineNodeTypes.LoopFromTool]: 460,
  [PipelineNodeTypes.Custom]: 570,
  [PipelineNodeTypes.Printer]: 400,
  [PipelineNodeTypes.Default]: 570,
  [PIPELINE_STATE]: 40,
  [PipelineNodeTypes.End]: 60,
  [PipelineNodeTypes.Mcp]: 630,
  [PipelineNodeTypes.Toolkit]: 460,
  [PipelineNodeTypes.Agent]: 460,
  [PipelineNodeTypes.Pipeline]: 340,
  [PipelineNodeTypes.Code]: 500,
  [PipelineNodeTypes.Parallel]: 750,
  [PipelineNodeTypes.Map]: 650,
  [PipelineNodeTypes.SplitOut]: 650,
  [PipelineNodeTypes.Aggregate]: 750,
  [PipelineNodeTypes.Router]: 550,
  [PipelineNodeTypes.Hitl]: 550,
  [PipelineNodeTypes.StateModifier]: 400,
  [PipelineNodeTypes.Ghost]: 60,
};

/** `PipelineNodeTypes` inverted (value -> declared key name), e.g. `'tool' -> 'Tool'`. */
export const PipelineNodeTypeNames: Readonly<Record<string, string>> = Object.entries(
  PipelineNodeTypes,
).reduce((acc, [key, value]) => ({ ...acc, [value]: key }), {});

export const NodeDisplayLabels: Readonly<Record<PipelineNodeType, string>> = {
  [PipelineNodeTypes.Tool]: 'Tool',
  [PipelineNodeTypes.Agent]: 'Agent',
  [PipelineNodeTypes.Pipeline]: 'Pipeline',
  [PipelineNodeTypes.Function]: 'Function',
  [PipelineNodeTypes.LLM]: 'LLM',
  [PipelineNodeTypes.Decision]: 'Decision',
  [PipelineNodeTypes.Condition]: 'Condition',
  [PipelineNodeTypes.Loop]: 'Loop',
  [PipelineNodeTypes.LoopFromTool]: 'Loop from tool',
  [PipelineNodeTypes.Router]: 'Router',
  [PipelineNodeTypes.StateModifier]: 'State modifier',
  [PipelineNodeTypes.Toolkit]: 'Toolkit',
  [PipelineNodeTypes.Mcp]: 'MCP',
  [PipelineNodeTypes.Code]: 'Code',
  [PipelineNodeTypes.Parallel]: 'Parallel',
  [PipelineNodeTypes.Map]: 'Map',
  [PipelineNodeTypes.SplitOut]: 'SplitOut',
  [PipelineNodeTypes.Aggregate]: 'Aggregate',
  [PipelineNodeTypes.Printer]: 'Printer',
  [PipelineNodeTypes.Hitl]: PipelineNodeDisplayNames[PipelineNodeTypes.Hitl],
  [PipelineNodeTypes.Custom]: 'Custom',
  [PipelineNodeTypes.Ghost]: 'Ghost',
  [PipelineNodeTypes.End]: 'END',
  [PipelineNodeTypes.Default]: 'Default',
};

export const ORIENTATION = {
  horizontal: 'horizontal',
  vertical: 'vertical',
} as const;

export type Orientation = (typeof ORIENTATION)[keyof typeof ORIENTATION];

export const OrientationKey = 'elitea_ui.orientation';

export const LAYOUT_VERSION = '1.0';

export const StatueTypeMap: Readonly<Record<string, string>> = {
  str: 'String',
  list: 'List',
  number: 'Number',
  dict: 'Json',
};

export const agentTaskTypeOptions = [
  { label: 'F-String', value: 'fstring' },
  { label: 'Variable', value: 'variable' },
  { label: 'Fixed', value: 'fixed' },
] as const;

export const FSTRING_AUTOCOMPLETE_VARIABLES: ReadonlySet<string> = new Set([
  'task',
  'system',
  'code',
  'user_message',
  'printer',
]);

export const InitialNodeId: Readonly<Record<string, string>> = {
  [PipelineNodeTypes.Tool]: 'Tool',
  [PipelineNodeTypes.Agent]: 'Agent',
  [PipelineNodeTypes.Pipeline]: 'Pipeline',
  [PipelineNodeTypes.LLM]: 'LLM',
  [PipelineNodeTypes.Code]: 'Code',
  [PipelineNodeTypes.Parallel]: 'Parallel',
  [PipelineNodeTypes.Map]: 'Map',
  [PipelineNodeTypes.SplitOut]: 'SplitOut',
  [PipelineNodeTypes.Aggregate]: 'Aggregate',
  [PipelineNodeTypes.Function]: 'Function',
  [PipelineNodeTypes.Condition]: 'Condition',
  [PipelineNodeTypes.Decision]: 'Decision',
  [PipelineNodeTypes.Loop]: 'Loop',
  [PipelineNodeTypes.End]: 'End',
  [PipelineNodeTypes.LoopFromTool]: 'LoopFromTool',
  [PipelineNodeTypes.Router]: 'Router',
  [PipelineNodeTypes.StateModifier]: 'StateModifier',
  [RUN_STATE_NODE]: 'RunState',
  [PipelineNodeTypes.Custom]: 'Custom',
  [PipelineNodeTypes.Ghost]: 'Ghost',
  [PipelineNodeTypes.Default]: 'Default',
  [PipelineNodeTypes.Toolkit]: 'Toolkit',
  [PipelineNodeTypes.Mcp]: 'MCP',
  [PipelineNodeTypes.Printer]: 'Printer',
  [PipelineNodeTypes.Hitl]: 'HITL',
};
