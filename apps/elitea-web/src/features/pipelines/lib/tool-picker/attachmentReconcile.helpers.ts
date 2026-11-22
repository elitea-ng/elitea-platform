import { getAttachmentKey, isSameToolSet } from './attachmentDiff.helpers';
import { getToolkitAvailableTools } from './attachmentTool.helpers';
import {
  collectToolReferences,
  getAttachmentReference,
  isApplicationTool,
} from './pipelineToolUsage.helpers';
import type {
  GetToolkitNameFn,
  PickerToolEntry,
  PipelineYamlDocument,
  ToolReference,
} from './toolPicker.types';

function getDesiredSelectedTools({
  tool,
  initialTool,
  ref,
  initialRef,
}: {
  readonly tool: PickerToolEntry;
  readonly initialTool: PickerToolEntry | undefined;
  readonly ref: ToolReference;
  readonly initialRef: ToolReference | undefined;
}): readonly (string | { readonly name?: string | undefined })[] | undefined {
  const usedTools = [...ref.toolNames].sort((a, b) => a.localeCompare(b));
  const initiallyUsedTools = initialRef ? [...initialRef.toolNames].sort((a, b) => a.localeCompare(b)) : [];

  if (initialTool && isSameToolSet(usedTools, initiallyUsedTools)) {
    return initialTool.settings?.selected_tools;
  }
  if (usedTools.length > 0) return usedTools;
  return initialTool ? initialTool.settings?.selected_tools : getToolkitAvailableTools(tool);
}

function reconcileSingleTool({
  tool,
  refs,
  initialRefs,
  initialByKey,
  getToolkitName,
}: {
  readonly tool: PickerToolEntry;
  readonly refs: ReadonlyMap<string, ToolReference>;
  readonly initialRefs: ReadonlyMap<string, ToolReference>;
  readonly initialByKey: ReadonlyMap<string, PickerToolEntry>;
  readonly getToolkitName: GetToolkitNameFn | undefined;
}): { readonly entry: PickerToolEntry | null; readonly changed: boolean } {
  const ref = getAttachmentReference(refs, tool, getToolkitName);
  const initialTool = initialByKey.get(getAttachmentKey(tool));
  const initialRef = initialTool
    ? getAttachmentReference(initialRefs, initialTool, getToolkitName)
    : undefined;

  if (!ref) {
    if (!initialTool || initialRef) return { entry: null, changed: true };
    return { entry: tool, changed: false };
  }

  if (isApplicationTool(tool)) {
    return { entry: tool, changed: false };
  }

  const desired = getDesiredSelectedTools({ tool, initialTool, ref, initialRef });
  if (desired !== undefined && !isSameToolSet(desired, tool.settings?.selected_tools)) {
    return {
      entry: { ...tool, settings: { ...tool.settings, selected_tools: desired } },
      changed: true,
    };
  }
  return { entry: tool, changed: false };
}

export interface ReconcileAttachmentsParams {
  readonly tools?: readonly PickerToolEntry[] | undefined;
  readonly initialTools?: readonly PickerToolEntry[] | undefined;
  readonly yamlJsonObject?: PipelineYamlDocument;
  readonly initialYamlJsonObject?: PipelineYamlDocument;
  readonly getToolkitName?: GetToolkitNameFn | undefined;
}

/**
 * Keeps `version_details.tools` in line with the nodes. Pure and idempotent.
 * Returns the same `tools` array reference when nothing changes.
 */
export function reconcileAttachments({
  tools = [],
  initialTools = [],
  yamlJsonObject,
  initialYamlJsonObject,
  getToolkitName,
}: ReconcileAttachmentsParams): readonly PickerToolEntry[] {
  const refs = collectToolReferences(yamlJsonObject);
  const initialRefs = collectToolReferences(initialYamlJsonObject);
  const initialByKey = new Map(initialTools.map((tool) => [getAttachmentKey(tool), tool]));

  let changed = false;
  const next: PickerToolEntry[] = [];

  for (const tool of tools) {
    const result = reconcileSingleTool({ tool, refs, initialRefs, initialByKey, getToolkitName });
    if (result.changed) changed = true;
    if (result.entry) next.push(result.entry);
  }

  return changed ? next : tools;
}
