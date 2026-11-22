import { PipelineNodeTypes, type PipelineNodeType } from '../flow-editor/constants/flowEditor.constants';
import type { PickerKind, ToolRefKind } from './toolPicker.types';

export const APPLICATION_TOOL_TYPE = 'application';

export const TOOL_REF_KINDS: Readonly<Record<ToolRefKind, ToolRefKind>> = {
  toolkit: 'toolkit',
  application: 'application',
};

/** Nodes whose `tool` (without `toolkit_name`) names an attached agent or pipeline. */
export const APPLICATION_REF_NODE_TYPES: readonly PipelineNodeType[] = [
  PipelineNodeTypes.Agent,
  PipelineNodeTypes.Pipeline,
  PipelineNodeTypes.Tool,
  PipelineNodeTypes.Loop,
  PipelineNodeTypes.LoopFromTool,
];

export const PICKER_KINDS: Readonly<Record<PickerKind, PickerKind>> = {
  toolkit: 'toolkit',
  mcp: 'mcp',
  agent: 'agent',
  pipeline: 'pipeline',
};

export const PICKER_PAGE_SIZE = 20;

export const PICKER_SEARCH_DEBOUNCE = 300;

/** Backend answers for a relation that is already in the requested state. */
export const IDEMPOTENT_RELATION_ERRORS: readonly string[] = [
  'Already exists relation',
  'Already removed relation',
];

/** Order of the attachments in the Tools section. */
export const PIPELINE_TOOL_KIND_ORDER: readonly PickerKind[] = [
  PICKER_KINDS.toolkit,
  PICKER_KINDS.mcp,
  PICKER_KINDS.agent,
  PICKER_KINDS.pipeline,
];

/** Card actions shown on card hover, as on the agent tool card. */
export const PIPELINE_TOOL_CARD_ACTION_CLASS = 'pipeline-tool-card-action';

/** Published and embedded versions cannot change their attachments (the backend rejects it). */
export const LOCKED_VERSION_STATUSES: readonly string[] = ['published', 'embedded'];
