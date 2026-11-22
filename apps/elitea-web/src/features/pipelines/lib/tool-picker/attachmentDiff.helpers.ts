import { isApplicationTool } from './pipelineToolUsage.helpers';
import type { AttachmentDiff, AttachmentVersionChange, PickerToolEntry } from './toolPicker.types';

/** Identity of an attachment across saves: the toolkit id, or the agent/pipeline id (a new one has no tool id yet). */
export function getAttachmentKey(tool: PickerToolEntry | null | undefined): string {
  if (isApplicationTool(tool)) {
    return `application:${String(tool?.settings?.application_id ?? '')}`;
  }
  return `toolkit:${String(tool?.id ?? '')}`;
}

export function isSameToolSet(
  a: readonly (string | { readonly name?: string | undefined })[] | undefined,
  b: readonly (string | { readonly name?: string | undefined })[] | undefined,
): boolean {
  const left = Array.isArray(a) ? a : [];
  const right = Array.isArray(b) ? b : [];
  if (left.length !== right.length) return false;
  const rightSet = new Set(right);
  return left.every((item) => rightSet.has(item));
}

/**
 * What Save must apply to the backend relations, keyed by attachment identity (not by list index).
 */
export function diffAttachments(
  initialTools: readonly PickerToolEntry[] = [],
  currentTools: readonly PickerToolEntry[] = [],
): AttachmentDiff {
  const initialByKey = new Map(initialTools.map((tool) => [getAttachmentKey(tool), tool]));
  const currentByKey = new Map(currentTools.map((tool) => [getAttachmentKey(tool), tool]));

  const added: PickerToolEntry[] = [];
  const selectedToolsChanged: PickerToolEntry[] = [];
  const versionChanged: AttachmentVersionChange[] = [];

  currentByKey.forEach((tool, key) => {
    const initialTool = initialByKey.get(key);
    if (!initialTool) {
      added.push(tool);
    } else if (isApplicationTool(tool)) {
      if (tool.settings?.application_version_id !== initialTool.settings?.application_version_id) {
        versionChanged.push({ tool, initialTool });
      }
    } else if (!isSameToolSet(tool.settings?.selected_tools, initialTool.settings?.selected_tools)) {
      selectedToolsChanged.push(tool);
    }
  });

  const removed = [...initialByKey.entries()]
    .filter(([key]) => !currentByKey.has(key))
    .map(([, tool]) => tool);

  return {
    added,
    removed,
    selectedToolsChanged,
    versionChanged,
    hasChanges: Boolean(
      added.length || removed.length || selectedToolsChanged.length || versionChanged.length,
    ),
  };
}
