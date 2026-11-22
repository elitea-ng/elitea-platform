import { toolkitDisplayName } from '@/entities/toolkit';

import type { PipelineNodeType } from '../flow-editor/constants/flowEditor.constants';
import {
  APPLICATION_REF_NODE_TYPES,
  APPLICATION_TOOL_TYPE,
  PICKER_KINDS,
  PIPELINE_TOOL_KIND_ORDER,
  TOOL_REF_KINDS,
} from './toolPicker.constants';
import type {
  AttachmentGroup,
  GetToolkitNameFn,
  PickerKind,
  PickerToolEntry,
  PipelineYamlDocument,
  ToolRefKind,
  ToolReference,
  ToolkitListRow,
} from './toolPicker.types';

export function isApplicationTool(tool: PickerToolEntry | ToolkitListRow | null | undefined): boolean {
  return tool?.type === APPLICATION_TOOL_TYPE;
}

/** Same split as the Toolkit and MCP nodes have always used. */
export function isMcpAttachment(tool: PickerToolEntry | ToolkitListRow | null | undefined): boolean {
  return !isApplicationTool(tool) && Boolean(tool?.meta?.mcp || tool?.type === 'mcp');
}

export function getToolRefKey(kind: ToolRefKind, name: string): string {
  return `${kind}:${name}`;
}

export function getReferencedNames(refs: ReadonlyMap<string, ToolReference>, kind: ToolRefKind): Set<string> {
  const names = new Set<string>();
  refs.forEach((ref) => {
    if (ref.kind === kind) {
      names.add(ref.name);
    }
  });
  return names;
}

/**
 * Name a node uses to bind an attachment: agents and pipelines by `name`, toolkits and MCPs by toolkit name.
 * `getToolkitName` resolves a toolkit without `toolkit_name` (see `useGetToolkitNameFromSchema`).
 */
export function getAttachmentRefName(
  tool: PickerToolEntry | ToolkitListRow | null | undefined,
  getToolkitName?: GetToolkitNameFn,
): string {
  if (!tool) return '';
  if (isApplicationTool(tool)) return tool.name ?? '';
  return tool.toolkit_name || getToolkitName?.(tool) || tool.name || '';
}

export function getAttachmentRefKey(
  tool: PickerToolEntry | ToolkitListRow | null | undefined,
  getToolkitName?: GetToolkitNameFn,
): string {
  return getToolRefKey(
    isApplicationTool(tool) ? TOOL_REF_KINDS.application : TOOL_REF_KINDS.toolkit,
    getAttachmentRefName(tool, getToolkitName),
  );
}

/**
 * Display label for a toolkit/MCP row or attachment, reusing `entities/toolkit`'s `toolkitDisplayName`.
 */
export function getToolkitDisplayName(tool: PickerToolEntry | ToolkitListRow | null | undefined): string {
  if (!tool) return '';
  return toolkitDisplayName({
    id: String(tool.id ?? ''),
    name: tool.name ?? tool.toolkit_name ?? '',
    type: tool.type ?? '',
  });
}

/**
 * The toolkit or MCP attachment a node binds to by `toolkitName`, or `undefined` when it is missing.
 * Also matches `name`, as the Toolkit/MCP node lookup always has (`useFunctionInputMapping`).
 */
export function findAttachedToolkit(
  tools: readonly PickerToolEntry[] | undefined,
  toolkitName: string | undefined,
  getToolkitName?: GetToolkitNameFn,
): PickerToolEntry | undefined {
  if (!toolkitName) return undefined;
  return (tools ?? []).find(
    (tool) =>
      !isApplicationTool(tool) &&
      (getAttachmentRefName(tool, getToolkitName) === toolkitName || tool.name === toolkitName),
  );
}

function addReference(
  refs: Map<string, ToolReference>,
  kind: ToolRefKind,
  name: unknown,
  nodeId: string,
  toolNames: readonly unknown[] = [],
): void {
  if (typeof name !== 'string' || !name) return;
  const key = getToolRefKey(kind, name);
  const ref = refs.get(key) ?? { kind, name, nodeIds: [], toolNames: new Set<string>() };
  if (!ref.nodeIds.includes(nodeId)) ref.nodeIds.push(nodeId);
  for (const toolName of toolNames) {
    if (typeof toolName === 'string' && toolName) {
      ref.toolNames.add(toolName);
    }
  }
  refs.set(key, ref);
}

function collectPrimaryToolRef(
  refs: Map<string, ToolReference>,
  node: Readonly<Record<string, unknown>>,
  nodeId: string,
): void {
  const toolkitName = node['toolkit_name'];
  const tool = node['tool'];
  const nodeType = node['type'] as PipelineNodeType | undefined;
  if (toolkitName) {
    addReference(refs, TOOL_REF_KINDS.toolkit, toolkitName, nodeId, tool ? [tool] : []);
  } else if (tool && nodeType && APPLICATION_REF_NODE_TYPES.includes(nodeType)) {
    addReference(refs, TOOL_REF_KINDS.application, tool, nodeId);
  }
}

function collectToolNamesMap(
  refs: Map<string, ToolReference>,
  toolNamesMap: unknown,
  nodeId: string,
): void {
  if (!toolNamesMap || typeof toolNamesMap !== 'object' || Array.isArray(toolNamesMap)) return;
  for (const [tkName, tkTools] of Object.entries(toolNamesMap as Record<string, unknown>)) {
    addReference(refs, TOOL_REF_KINDS.toolkit, tkName, nodeId, Array.isArray(tkTools) ? tkTools : []);
  }
}

function collectNodeReferences(refs: Map<string, ToolReference>, node: Readonly<Record<string, unknown>>): void {
  const nodeId = node['id'];
  if (typeof nodeId !== 'string' || !nodeId) return;

  collectPrimaryToolRef(refs, node, nodeId);

  const loopToolkitName = node['loop_toolkit_name'];
  if (loopToolkitName) {
    const loopTool = node['loop_tool'];
    addReference(refs, TOOL_REF_KINDS.toolkit, loopToolkitName, nodeId, loopTool ? [loopTool] : []);
  }

  collectToolNamesMap(refs, node['tool_names'], nodeId);
}

/**
 * Toolkits, MCPs, agents and pipelines referenced by the pipeline nodes.
 */
export function collectToolReferences(yamlJsonObject: PipelineYamlDocument): Map<string, ToolReference> {
  const refs = new Map<string, ToolReference>();
  const nodes = yamlJsonObject?.nodes;
  if (!Array.isArray(nodes)) return refs;
  for (const node of nodes) {
    if (node && typeof node === 'object') {
      collectNodeReferences(refs, node as Readonly<Record<string, unknown>>);
    }
  }
  return refs;
}

export function getAttachmentReference(
  refs: ReadonlyMap<string, ToolReference>,
  tool: PickerToolEntry,
  getToolkitName?: GetToolkitNameFn,
): ToolReference | undefined {
  return refs.get(getAttachmentRefKey(tool, getToolkitName));
}

export function getAttachmentKind(tool: PickerToolEntry): PickerKind {
  if (isApplicationTool(tool)) {
    return tool.agent_type === 'pipeline' ? PICKER_KINDS.pipeline : PICKER_KINDS.agent;
  }
  return isMcpAttachment(tool) ? PICKER_KINDS.mcp : PICKER_KINDS.toolkit;
}

export interface GroupAttachmentsByKindParams {
  readonly tools?: readonly PickerToolEntry[] | undefined;
  readonly refs?: ReadonlyMap<string, ToolReference> | undefined;
  readonly getToolkitName?: GetToolkitNameFn | undefined;
  readonly hiddenKinds?: readonly PickerKind[] | undefined;
  readonly hiddenToolIds?: readonly (number | string)[] | undefined;
}

/**
 * Attachments by kind, in toolkits / MCPs / agents / pipelines order, each with the nodes that use it.
 * Empty groups are left out.
 */
export function groupAttachmentsByKind({
  tools = [],
  refs = new Map(),
  getToolkitName,
  hiddenKinds = [],
  hiddenToolIds = [],
}: GroupAttachmentsByKindParams): AttachmentGroup[] {
  return PIPELINE_TOOL_KIND_ORDER.filter((kind) => !hiddenKinds.includes(kind))
    .map((kind) => ({
      kind,
      items: tools
        .filter(
          (tool) =>
            getAttachmentKind(tool) === kind &&
            (tool.id === undefined || !hiddenToolIds.includes(tool.id)),
        )
        .map((tool) => ({
          key: `${getAttachmentRefKey(tool, getToolkitName)}:${String(tool.id ?? tool.settings?.application_id ?? '')}`,
          tool,
          kind,
          usedBy: getAttachmentReference(refs, tool, getToolkitName)?.nodeIds ?? [],
        })),
    }))
    .filter((group) => group.items.length > 0);
}
