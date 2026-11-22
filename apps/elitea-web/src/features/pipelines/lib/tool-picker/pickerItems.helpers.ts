import { getAgentDisabledReason } from './agentNesting.helpers';
import { getAttachmentRefName, isApplicationTool } from './pipelineToolUsage.helpers';
import type {
  ApplicationListRow,
  GetToolkitNameFn,
  PickerItem,
  PickerToolEntry,
  ToolkitListRow,
} from './toolPicker.types';

export function DUPLICATE_NAME_REASON(name: string): string {
  return `Another toolkit is also named "${name}". Nodes bind toolkits by name, so rename one of them to use it.`;
}

export function INTERRUPT_REASON(name: string): string {
  return `The ${name} pipeline can't be used as it contains interruptions in subgraphs, which are not allowed.`;
}

export function DUPLICATE_APPLICATION_NAME_REASON(name: string): string {
  return `The pipeline already uses another agent or pipeline named "${name}". Nodes bind them by name, so rename one of them to use it.`;
}

function matchesQuery(item: Pick<PickerItem, 'label' | 'name'>, query: string): boolean {
  const text = query.trim().toLowerCase();
  if (!text) return true;
  return [item.label, item.name].some((value) => value.toLowerCase().includes(text));
}

export interface BuildToolkitPickerItemsParams {
  readonly rows?: readonly ToolkitListRow[] | undefined;
  readonly attachedTools?: readonly PickerToolEntry[] | undefined;
  readonly usedNames?: ReadonlySet<string> | undefined;
  readonly query?: string | undefined;
  readonly getToolkitName?: GetToolkitNameFn | undefined;
  readonly getLabel?: ((source: PickerToolEntry | ToolkitListRow) => string) | undefined;
}

/**
 * Picker items for toolkits or MCPs. Toolkits used by the pipeline nodes come first (from the attachments,
 * so they show even when not loaded yet); a toolkit whose name another toolkit already has is disabled.
 */
export function buildToolkitPickerItems({
  rows = [],
  attachedTools = [],
  usedNames = new Set(),
  query = '',
  getToolkitName,
  getLabel = (source) => source.name ?? '',
}: BuildToolkitPickerItemsParams): PickerItem<PickerToolEntry | ToolkitListRow>[] {
  const toItem = (
    source: PickerToolEntry | ToolkitListRow,
    isAttached: boolean,
  ): PickerItem<PickerToolEntry | ToolkitListRow> => {
    const name = getAttachmentRefName(source, getToolkitName);
    return {
      key: `toolkit:${String(source.id ?? name)}`,
      id: source.id,
      name,
      label: getLabel(source) || name,
      description: source.description ?? '',
      source,
      isAttached,
      inUse: usedNames.has(name),
      disabledReason: '',
    };
  };

  const ownerIdByName = new Map<string, number | string | undefined>();
  const claimName = (item: PickerItem<PickerToolEntry | ToolkitListRow>): boolean => {
    const ownerId = ownerIdByName.get(item.name);
    if (ownerId === undefined) {
      ownerIdByName.set(item.name, item.id);
      return true;
    }
    return ownerId === item.id;
  };

  const attachedItems = attachedTools
    .filter((tool) => !isApplicationTool(tool))
    .map((tool) => toItem(tool, true));
  attachedItems.forEach(claimName);
  const attachedIds = new Set(attachedItems.map((item) => item.id));

  const pinned = attachedItems.filter((item) => item.inUse && matchesQuery(item, query));
  const listed = rows
    .filter((row) => !attachedIds.has(row.id) || !usedNames.has(getAttachmentRefName(row, getToolkitName)))
    .map((row) => {
      const item: PickerItem<PickerToolEntry | ToolkitListRow> = {
        ...toItem(row, attachedIds.has(row.id)),
        inUse: false,
      };
      return claimName(item) ? item : { ...item, disabledReason: DUPLICATE_NAME_REASON(item.name) };
    });

  return [...pinned, ...listed];
}

export interface BuildApplicationPickerItemsParams {
  readonly rows?: readonly ApplicationListRow[] | undefined;
  readonly attachedTools?: readonly PickerToolEntry[] | undefined;
  readonly allAttachedTools?: readonly PickerToolEntry[] | undefined;
  readonly usedNames?: ReadonlySet<string> | undefined;
  readonly query?: string | undefined;
  readonly excludeId?: number | string | undefined;
  readonly hostTier?: number | undefined;
}

/**
 * Picker items for agents or pipelines; the pipeline being edited is left out and
 * Swarm / over-nested / interrupting / duplicate-named candidates are disabled with a reason.
 */
export function buildApplicationPickerItems({
  rows = [],
  attachedTools = [],
  allAttachedTools = attachedTools,
  usedNames = new Set(),
  query = '',
  excludeId,
  hostTier,
}: BuildApplicationPickerItemsParams): PickerItem<PickerToolEntry | ApplicationListRow>[] {
  const attachedIdByName = new Map<string, number | undefined>(
    allAttachedTools
      .filter(isApplicationTool)
      .map((tool) => [tool.name ?? '', tool.settings?.application_id]),
  );

  const getDisabledReason = (row: ApplicationListRow): string => {
    if (row.has_interrupt) return INTERRUPT_REASON(row.name);
    const agentReason = getAgentDisabledReason(row, hostTier);
    if (agentReason) return agentReason;
    const attachedId = attachedIdByName.get(row.name);
    return attachedId !== undefined && attachedId !== row.id
      ? DUPLICATE_APPLICATION_NAME_REASON(row.name)
      : '';
  };

  const pinned: PickerItem<PickerToolEntry | ApplicationListRow>[] = attachedTools
    .filter((tool) => isApplicationTool(tool) && Boolean(tool.name) && usedNames.has(tool.name ?? ''))
    .map((tool) => ({
      key: `application:${String(tool.settings?.application_id ?? tool.id ?? tool.name ?? '')}`,
      id: tool.settings?.application_id,
      name: tool.name ?? '',
      label: tool.name ?? '',
      description: tool.description ?? '',
      iconMeta: tool.icon_meta,
      source: tool,
      isAttached: true,
      inUse: true,
      disabledReason: '',
    }))
    .filter((item) => matchesQuery(item, query));

  const pinnedIds = new Set(pinned.map((item) => item.id));

  const listed: PickerItem<PickerToolEntry | ApplicationListRow>[] = rows
    .filter((row) => row.id !== excludeId && !pinnedIds.has(row.id))
    .map((row) => ({
      key: `application:${String(row.id)}`,
      id: row.id,
      name: row.name,
      label: row.name,
      description: row.description ?? '',
      iconMeta: row.icon_meta,
      source: row,
      isAttached: false,
      inUse: false,
      disabledReason: getDisabledReason(row),
    }));

  return [...pinned, ...listed];
}
