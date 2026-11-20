import { APPLICATION_TOOL_TYPE } from './toolPicker.constants';
import type { ApplicationListRow, PickerToolEntry, ToolkitListRow } from './toolPicker.types';

const SAFE_SEGMENT_RE = /^[A-Za-z0-9_-]+$/;

export function encodeSafePathSegment(raw: number | string | undefined): string | null {
  if (raw === undefined || raw === null) return null;
  const text = String(raw);
  if (!text || !SAFE_SEGMENT_RE.test(text)) return null;
  return encodeURIComponent(text);
}

export interface BuildToolkitAttachmentOptions {
  readonly mcp?: boolean | undefined;
  readonly toolkitName?: string | undefined;
}

function resolveToolkitDisplayName(
  toolkit: ToolkitListRow | PickerToolEntry,
  toolkitName: string | undefined,
): string {
  return toolkit.name || toolkitName || toolkit.type || 'New Toolkit';
}

/**
 * A toolkit or MCP entry for `version_details.tools`, shaped like the one toolkit association adds.
 * `toolkitName` must be the name the node writes into `toolkit_name`, so the two stay bound.
 */
export function buildToolkitAttachment(
  toolkit: ToolkitListRow | PickerToolEntry,
  { mcp = false, toolkitName }: BuildToolkitAttachmentOptions = {},
): PickerToolEntry {
  const baseSettings = toolkit.settings;
  return {
    id: toolkit.id,
    type: toolkit.type || 'custom',
    name: resolveToolkitDisplayName(toolkit, toolkitName),
    description: toolkit.description ?? '',
    settings: {
      ...baseSettings,
      available_tools: baseSettings?.selected_tools ?? [],
    },
    toolkit_name: toolkitName || toolkit.toolkit_name || toolkit.name,
    online: toolkit.online,
    meta: { mcp },
    ...(toolkit.icon_meta ? { icon_meta: toolkit.icon_meta } : {}),
    author: toolkit.author ?? {},
    author_id: toolkit.author?.id,
  };
}

function resolveApplicationSettings(
  id: number | string | undefined,
  versionId: number | undefined,
): PickerToolEntry['settings'] {
  const numericId = Number(id);
  return {
    application_id: Number.isFinite(numericId) ? numericId : undefined,
    application_version_id: versionId,
  };
}

function resolveVersionVariables(
  variables: readonly { readonly name: string; readonly value: string }[] | undefined,
): readonly { readonly name: string; readonly value: string }[] {
  return (variables ?? []).map(({ name, value }) => ({ name, value }));
}

/** An agent or pipeline entry. Has no tool id until Save applies the relation. */
export function buildApplicationAttachment(
  application: ApplicationListRow | PickerToolEntry,
  versionDetails?: PickerToolEntry['version_details'],
): PickerToolEntry {
  const details = versionDetails ?? application.version_details;
  const author = details?.author ?? {};
  return {
    type: APPLICATION_TOOL_TYPE,
    name: application.name ?? '',
    description: application.description ?? '',
    agent_type: details?.agent_type ?? application.agent_type,
    icon_meta: details?.icon_meta ?? {},
    settings: resolveApplicationSettings(application.id, details?.id),
    author,
    author_id: author.id,
    variables: resolveVersionVariables(details?.variables),
  };
}

export function withApplicationVersion(
  tool: PickerToolEntry,
  versionDetails: { readonly id?: number | undefined; readonly icon_meta?: Readonly<Record<string, unknown>> | undefined } | undefined,
): PickerToolEntry {
  return {
    ...tool,
    ...(versionDetails?.icon_meta ? { icon_meta: versionDetails.icon_meta } : {}),
    settings: {
      ...tool.settings,
      application_version_id: versionDetails?.id,
    },
  };
}

type ToolNameEntry = string | { readonly name?: string | undefined };

/** Tools a toolkit offers, before the pipeline narrows `selected_tools` to the ones its nodes use. */
export function getToolkitAvailableTools(
  tool: PickerToolEntry | null | undefined,
): readonly ToolNameEntry[] {
  const available = tool?.settings?.available_tools;
  if (available && available.length > 0) return available;
  const selected = tool?.settings?.selected_tools;
  return selected && selected.length > 0 ? selected : [];
}

/**
 * Page path of an attached item, validating every path segment against traversal/CRLF/encoded slashes.
 */
export function getAttachmentPagePath(tool: PickerToolEntry | null | undefined): string | null {
  if (!tool) return null;
  if (tool.type === APPLICATION_TOOL_TYPE) {
    const applicationSegment = encodeSafePathSegment(tool.settings?.application_id);
    if (!applicationSegment) return null;
    const section = tool.agent_type === 'pipeline' ? 'pipelines' : 'agents';
    const versionSegment = encodeSafePathSegment(tool.settings?.application_version_id);
    return `/${section}/all/${applicationSegment}${versionSegment ? `/${versionSegment}` : ''}`;
  }
  const toolkitSegment = encodeSafePathSegment(tool.id);
  if (!toolkitSegment) return null;
  const isMcp = Boolean(tool.meta?.mcp || tool.type === 'mcp');
  return `/${isMcp ? 'mcps' : 'toolkits'}/all/${toolkitSegment}`;
}
