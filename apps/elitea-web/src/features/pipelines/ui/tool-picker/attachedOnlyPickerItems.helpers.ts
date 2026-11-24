import { getAttachmentRefName, isApplicationTool } from '../../lib/tool-picker/pipelineToolUsage.helpers';
import type {
  ApplicationListRow,
  GetToolkitNameFn,
  PickerItem,
  PickerToolEntry,
  ToolkitListRow,
} from '../../lib/tool-picker/toolPicker.types';

function matchesPickerQuery(item: Pick<PickerItem, 'label' | 'name'>, query: string): boolean {
  const text = query.trim().toLowerCase();
  if (!text) return true;
  return [item.label, item.name].some((value) => value.toLowerCase().includes(text));
}

function dedupeAndSortInUseFirst<T extends PickerItem>(items: readonly T[], query: string): T[] {
  const seenKeys = new Set<string>();
  const filtered: T[] = [];
  for (const item of items) {
    if (seenKeys.has(item.key)) continue;
    seenKeys.add(item.key);
    if (matchesPickerQuery(item, query)) filtered.push(item);
  }
  return [...filtered.filter((item) => item.inUse), ...filtered.filter((item) => !item.inUse)];
}

function isExcludedId(
  resolvedId: number | string | undefined,
  excludeId: number | string | undefined,
): boolean {
  return excludeId !== undefined && resolvedId !== undefined && String(resolvedId) === String(excludeId);
}

function toAttachedApplicationItem(
  tool: PickerToolEntry,
  usedNames: ReadonlySet<string>,
  pipelineGroup: boolean,
): PickerItem<PickerToolEntry | ApplicationListRow> {
  const name = tool.name ?? '';
  const resolvedId = tool.settings?.application_id ?? tool.id;
  return {
    key: `application:${String(resolvedId ?? name)}`,
    id: resolvedId,
    name,
    label: name,
    description: tool.description ?? '',
    iconMeta: tool.icon_meta,
    source: tool,
    isAttached: true,
    inUse: usedNames.has(name),
    isPipeline: pipelineGroup,
    disabledReason: '',
  };
}

function toAttachedToolkitItem(
  tool: PickerToolEntry,
  usedNames: ReadonlySet<string>,
  getToolkitName: GetToolkitNameFn | undefined,
  getLabel: (source: PickerToolEntry | ToolkitListRow) => string,
  mcp: boolean | undefined,
): PickerItem<PickerToolEntry | ToolkitListRow> {
  const name = getAttachmentRefName(tool, getToolkitName);
  return {
    key: `toolkit:${String(tool.id ?? name)}`,
    id: tool.id,
    name,
    label: getLabel(tool) || name,
    description: tool.description ?? '',
    source: tool,
    isAttached: true,
    inUse: usedNames.has(name),
    disabledReason: '',
    ...(mcp !== undefined ? { mcp } : {}),
  };
}

export interface BuildAttachedApplicationPickerItemsParams {
  readonly attachedTools?: readonly PickerToolEntry[] | undefined;
  readonly usedNames?: ReadonlySet<string> | undefined;
  readonly query?: string | undefined;
  readonly excludeId?: number | string | undefined;
  readonly pipelineGroup: boolean;
}

/**
 * Builds picker items strictly from already-attached agents/pipelines (`attachedOnly` mode).
 * Normalizes both wire-shaped (`id: string`, no `settings.application_id`) and attachment-shaped
 * (`settings.application_id: number`, `id: undefined`) entries without self-collision or duplication,
 * and filters all attached entries client-side by `query`.
 */
export function buildAttachedApplicationPickerItems({
  attachedTools = [],
  usedNames = new Set(),
  query = '',
  excludeId,
  pipelineGroup,
}: BuildAttachedApplicationPickerItemsParams): PickerItem<PickerToolEntry | ApplicationListRow>[] {
  const items = attachedTools
    .filter((tool) => isApplicationTool(tool) && Boolean(tool.name))
    .map((tool) => toAttachedApplicationItem(tool, usedNames, pipelineGroup))
    .filter((item) => !isExcludedId(item.id, excludeId));

  return dedupeAndSortInUseFirst(items, query);
}

export interface BuildAttachedToolkitPickerItemsParams {
  readonly attachedTools?: readonly PickerToolEntry[] | undefined;
  readonly usedNames?: ReadonlySet<string> | undefined;
  readonly query?: string | undefined;
  readonly getToolkitName?: GetToolkitNameFn | undefined;
  readonly getLabel?: ((source: PickerToolEntry | ToolkitListRow) => string) | undefined;
  readonly mcp?: boolean | undefined;
}

/**
 * Builds picker items strictly from already-attached toolkits/MCPs (`attachedOnly` mode),
 * placing in-use entries first and filtering all entries client-side by `query`.
 */
export function buildAttachedToolkitPickerItems({
  attachedTools = [],
  usedNames = new Set(),
  query = '',
  getToolkitName,
  getLabel = (source) => source.name ?? '',
  mcp,
}: BuildAttachedToolkitPickerItemsParams): PickerItem<PickerToolEntry | ToolkitListRow>[] {
  const items = attachedTools
    .filter((tool) => !isApplicationTool(tool))
    .map((tool) => toAttachedToolkitItem(tool, usedNames, getToolkitName, getLabel, mcp));

  return dedupeAndSortInUseFirst(items, query);
}
