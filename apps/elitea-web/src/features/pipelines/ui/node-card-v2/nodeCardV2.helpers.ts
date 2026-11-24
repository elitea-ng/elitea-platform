import { PipelineNodeDisplayNames, PipelineNodeTypes } from '../../lib/flow-editor/constants/flowEditor.constants';
import type { YamlPipelineNode } from '../../lib/flow-editor/helpers/pipelineFlow.types';

/** Zoom threshold below which `NodeCardView` collapses into its compact mini-card mode. */
export const MINI_CARD_ZOOM = 0.6;

/** Fixed width of the compact view-only card in rem (`280px` at 16px root). */
export const NODE_CARD_V2_WIDTH_REM = '17.5rem';

export type NodeCardV2Status = 'running' | 'error' | 'needsConfiguration' | null;

export interface NodeCardV2StatusInput {
  readonly isPerforming?: boolean | undefined;
  readonly hasRunError?: boolean | undefined;
  readonly hasConfigIssues?: boolean | undefined;
  readonly hasAdmissionIssues?: boolean | undefined;
}

export interface NodeCardV2SummaryData {
  readonly primary: string;
  readonly secondary: string;
}

export interface NodeCardV2SummaryOptions {
  readonly type: string;
  readonly isDeprecated?: boolean | undefined;
  readonly requiredInputs?: readonly string[] | undefined;
}

export function isMiniCardZoom(zoom: number): boolean {
  return zoom < MINI_CARD_ZOOM;
}

export function getNodeCardV2Status(input: NodeCardV2StatusInput = {}): NodeCardV2Status {
  if (input.isPerforming) return 'running';
  if (input.hasRunError) return 'error';
  if (input.hasConfigIssues || input.hasAdmissionIssues) return 'needsConfiguration';
  return null;
}

function pluralize(count: number, word: string, pluralWord = `${word}s`): string {
  return `${count} ${count === 1 ? word : pluralWord}`;
}

export function toPreviewText(value: unknown): string {
  if (value === undefined || value === null) return '';
  const text = typeof value === 'string' ? value : JSON.stringify(value);
  return text.replace(/\s+/g, ' ').trim();
}

function getFirstLine(value: unknown): string {
  if (typeof value !== 'string') return toPreviewText(value);
  return value.split('\n').find((line) => line.trim())?.trim() ?? '';
}

function joinList(list: unknown): string {
  return Array.isArray(list) ? list.filter((item): item is string => typeof item === 'string' && item.length > 0).join(', ') : '';
}

function isEmptyMappingValue(entry: unknown): boolean {
  if (entry === undefined || entry === null) return true;
  if (typeof entry === 'string') return entry.trim().length === 0;
  if (typeof entry === 'object' && 'value' in entry) {
    const val = (entry as { readonly value?: unknown }).value;
    return val === undefined || val === null || (typeof val === 'string' && val.trim().length === 0);
  }
  return false;
}

function readRecord(value: unknown): Readonly<Record<string, unknown>> | undefined {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    ? (value as Readonly<Record<string, unknown>>)
    : undefined;
}

function readInputTask(yamlNode: YamlPipelineNode): unknown {
  const mapping = readRecord(yamlNode.input_mapping);
  const task = readRecord(mapping?.['task']);
  return task?.['value'];
}

function getToolNamesSummary(toolNames: unknown): string {
  const record = readRecord(toolNames);
  if (!record) return '';
  const toolkits = Object.keys(record);
  if (!toolkits.length) return '';
  const toolsCount = toolkits.reduce((count, toolkit) => {
    const list = record[toolkit];
    return count + (Array.isArray(list) ? list.length : 0);
  }, 0);
  return `${pluralize(toolkits.length, 'toolkit')} · ${pluralize(toolsCount, 'tool')}`;
}

function getParamsSummary(yamlNode: YamlPipelineNode, requiredInputs: readonly string[]): string {
  if (!requiredInputs.length) return '';
  const mapping = readRecord(yamlNode.input_mapping);
  const setCount = requiredInputs.filter((key) => !isEmptyMappingValue(mapping?.[key])).length;
  return `Params ${setCount}/${requiredInputs.length} set`;
}

function getHitlRoutesSummary(routesValue: unknown): string {
  const routes = readRecord(routesValue);
  if (!routes) return '';
  const pairs: readonly [string, unknown][] = [
    ['Approve', routes['approve']],
    ['Edit', routes['edit']],
    ['Reject', routes['reject']],
  ];
  return pairs
    .filter(([, target]) => typeof target === 'string' && target.length > 0)
    .map(([label, target]) => `${label} → ${String(target)}`)
    .join(' · ');
}

function getMapSummary(yamlNode: YamlPipelineNode): NodeCardV2SummaryData {
  const source = toPreviewText(yamlNode['source']);
  const worker = toPreviewText(yamlNode['worker']);
  const destination = toPreviewText(yamlNode['destination']);
  const secondary = worker ? `Worker → ${worker}` : destination && source ? `→ ${destination}` : '';
  return { primary: source || destination, secondary };
}

function getParallelSummary(yamlNode: YamlPipelineNode): NodeCardV2SummaryData {
  const branches = Array.isArray(yamlNode['branches']) ? yamlNode['branches'] : [];
  return {
    primary: branches.length ? pluralize(branches.length, 'branch', 'branches') : '',
    secondary: toPreviewText(yamlNode['join']) || joinList(yamlNode['output']),
  };
}

function getSplitOutSummary(yamlNode: YamlPipelineNode): NodeCardV2SummaryData {
  return {
    primary: toPreviewText(yamlNode['source']),
    secondary: toPreviewText(yamlNode['destination']) || joinList(yamlNode['output']),
  };
}

function getAggregateSummary(yamlNode: YamlPipelineNode): NodeCardV2SummaryData {
  const ops = Array.isArray(yamlNode['operations']) ? yamlNode['operations'] : [];
  return {
    primary: toPreviewText(yamlNode['source']),
    secondary: ops.length ? pluralize(ops.length, 'operation') : joinList(yamlNode['output']),
  };
}

const EXTENSION_SUMMARY_EXTRACTORS: Readonly<Record<string, (n: YamlPipelineNode) => NodeCardV2SummaryData>> = {
  [PipelineNodeTypes.Map]: getMapSummary,
  [PipelineNodeTypes.Parallel]: getParallelSummary,
  [PipelineNodeTypes.SplitOut]: getSplitOutSummary,
  [PipelineNodeTypes.Aggregate]: getAggregateSummary,
};

function getExtensionSummary(yamlNode: YamlPipelineNode): NodeCardV2SummaryData | null {
  const nodeType = typeof yamlNode.type === 'string' ? yamlNode.type : '';
  const extractor = Object.hasOwn(EXTENSION_SUMMARY_EXTRACTORS, nodeType)
    ? EXTENSION_SUMMARY_EXTRACTORS[nodeType]
    : undefined;
  return extractor ? extractor(yamlNode) : null;
}

type CoreSummaryExtractor = (yamlNode: YamlPipelineNode, requiredInputs: readonly string[]) => NodeCardV2SummaryData;

function getToolNodeSummary(yamlNode: YamlPipelineNode, requiredInputs: readonly string[]): NodeCardV2SummaryData {
  const toolkit = toPreviewText(yamlNode['toolkit_name']);
  const tool = toPreviewText(yamlNode['tool']);
  return {
    primary: toolkit && tool ? `${toolkit} › ${tool}` : toolkit || tool,
    secondary: getParamsSummary(yamlNode, requiredInputs),
  };
}

const CORE_SUMMARY_EXTRACTORS: Readonly<Record<string, CoreSummaryExtractor>> = {
  [PipelineNodeTypes.Agent]: (n) => ({ primary: toPreviewText(n['tool']), secondary: toPreviewText(readInputTask(n)) }),
  [PipelineNodeTypes.Code]: (n) => ({ primary: getFirstLine(readRecord(n['code'])?.['value']), secondary: joinList(n['output']) }),
  [PipelineNodeTypes.Decision]: (n) => {
    const nodes = Array.isArray(n.nodes) ? n.nodes : [];
    return { primary: toPreviewText(n['description']), secondary: nodes.length ? pluralize(nodes.length, 'output') : '' };
  },
  [PipelineNodeTypes.Hitl]: (n) => ({ primary: toPreviewText(readRecord(n['user_message'])?.['value']), secondary: getHitlRoutesSummary(n['routes']) }),
  [PipelineNodeTypes.LLM]: (n) => ({ primary: toPreviewText(readInputTask(n)), secondary: getToolNamesSummary(n['tool_names']) }),
  [PipelineNodeTypes.Mcp]: getToolNodeSummary,
  [PipelineNodeTypes.Toolkit]: getToolNodeSummary,
  [PipelineNodeTypes.Printer]: (n) => ({ primary: toPreviewText(n['final_message']), secondary: '' }),
  [PipelineNodeTypes.Router]: (n) => {
    const routes = Array.isArray(n['routes']) ? n['routes'] : [];
    return { primary: toPreviewText(n.condition), secondary: routes.length ? pluralize(routes.length, 'route') : '' };
  },
  [PipelineNodeTypes.StateModifier]: (n) => ({ primary: toPreviewText(n['template']), secondary: joinList(n['output']) }),
};

function getCoreSummary(yamlNode: YamlPipelineNode, requiredInputs: readonly string[]): NodeCardV2SummaryData {
  const nodeType = typeof yamlNode.type === 'string' ? yamlNode.type : '';
  const extractor = Object.hasOwn(CORE_SUMMARY_EXTRACTORS, nodeType) ? CORE_SUMMARY_EXTRACTORS[nodeType] : undefined;
  return extractor ? extractor(yamlNode, requiredInputs) : { primary: '', secondary: '' };
}

/**
 * Computes two compact read-only summary lines for all 14 admitted replatform node types
 * (plus deprecated nodes). Pure function safe for unit tests and canvas rendering.
 */
export function getNodeCardV2Summary(
  yamlNode: YamlPipelineNode | undefined,
  options: NodeCardV2SummaryOptions,
): NodeCardV2SummaryData {
  const { type, isDeprecated = false, requiredInputs = [] } = options;
  if (isDeprecated) {
    const displayNames = PipelineNodeDisplayNames as Readonly<Record<string, string>>;
    const displayName = Object.hasOwn(displayNames, type) ? displayNames[type]! : type;
    return { primary: displayName, secondary: 'Deprecated' };
  }
  if (!yamlNode) return { primary: '', secondary: '' };
  return getExtensionSummary(yamlNode) ?? getCoreSummary(yamlNode, requiredInputs);
}
