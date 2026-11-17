import type { VersionToolRef } from '@/shared/api/generated/model';

import type { AgentToolAssociation } from './types';

/**
 * `version_details.tools[]` as the server sends it (`VersionToolRef`) ->
 * this slice's own `AgentToolAssociation` display shape.
 *
 * Not a rename pass: the wire row and the shape `ToolCard` was ported
 * against disagree in two places, both verified against the generated
 * schema's own `NOTE(W2)` evidence comment and the Go handler it cites
 * (`applications/handler.go`'s `fetchVersionDetails`), not guessed:
 *  - The toolkit's own settings blob arrives under **`config`** for
 *    `entity_tool_mapping` rows (the joined `elitea_tools.settings`) and
 *    under **`settings`** for legacy `application_tools` rows. `ToolCard`
 *    and everything under it read `tool.settings` only — a row whose blob
 *    came back as `config` would render with no URL, no available-tools
 *    list and no `application_id`, i.e. an "open in new tab" button that
 *    goes nowhere.
 *  - `selected_tools` is a TOP-LEVEL wire field, while `ToolCard.lib.ts`
 *    reads it from `tool.settings.selected_tools` — it is folded in here.
 *
 * `variables` is deliberately absent: `fetchVersionDetails` emits no
 * per-tool variables at all (the tool row it builds has exactly
 * `id`/`tool_id`/`entity_type`/`selected_tools`/`name`/`type`/`config`), so
 * `ToolCard`'s variables toggle correctly never appears for a real row.
 */
function asRecord(value: unknown): Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as Record<string, unknown>) : {};
}

function asStringArray(value: unknown): readonly string[] | undefined {
  if (!Array.isArray(value)) return undefined;
  return value.filter((entry): entry is string => typeof entry === 'string');
}

/** Un-exported: only `toAgentToolAssociations` below calls it, and knip flags an unused named export. */
function toAgentToolAssociation(row: VersionToolRef): AgentToolAssociation {
  const blob = asRecord(row.config ?? row.settings);
  const selectedTools = asStringArray(row.selected_tools);
  // One cast, over an opaque passthrough blob the generated schema types as
  // `unknown` on purpose (`ToolSettings` = `zod.unknown()`): every field
  // `AgentToolSettings` names is optional, so the merge below cannot produce
  // a structurally invalid value — it can only carry keys nothing reads.
  // The toolkit's own enabled tools are what THIS agent may choose from, and
  // the attachment's `selected_tools` (merged over them below) is the subset it
  // uses. The runtime composes the same pair (`available_tools` = the toolkit's
  // list, `selected_tools` = the attachment's); without `available_tools` the
  // card had no options to offer and no way to filter tools per agent (#2146).
  const toolkitTools = asStringArray(blob['selected_tools']);
  const settings = {
    ...blob,
    ...(toolkitTools === undefined || toolkitTools.length === 0 || blob['available_tools'] !== undefined
      ? {}
      : { available_tools: toolkitTools }),
    ...(selectedTools === undefined ? {} : { selected_tools: selectedTools }),
  } as NonNullable<AgentToolAssociation['settings']>;

  return {
    id: row.id,
    ...(row.tool_id === undefined ? {} : { tool_id: row.tool_id }),
    ...(row.type === undefined ? {} : { type: row.type }),
    ...(row.name === undefined ? {} : { name: row.name }),
    settings,
  };
}

export function toAgentToolAssociations(rows: readonly VersionToolRef[] | undefined): readonly AgentToolAssociation[] {
  return (rows ?? []).map(toAgentToolAssociation);
}
