import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseSwitch } from '@/shared/ui/BaseSwitch';
import { InfoTooltip } from '@/shared/ui/InfoTooltip';

import type { EditToolDetail, EditToolField, ToolSchema } from './types';

/**
 * The ONE thing `resolveMcpExposureField` needs off `ToolBase.render.tsx`'s
 * `PropertyPassParams`, restated structurally rather than imported.
 *
 * Importing the type from `ToolBase.render.tsx` — which imports THIS module
 * for its two functions — is a circular import, and `import type` does not
 * make it harmless: the bundler still emits the cycle, and the value side of
 * it initialises as `undefined`. Measured: the whole toolkit editor rendered
 * an empty left panel, with no console error and no page error, on every
 * toolkit type (`check-layer-cycle` names the cycle; the screen just goes
 * blank). TypeScript checks this structurally against the real
 * `PropertyPassParams` at the call site, so nothing is loosened.
 */
interface McpExposurePassParams {
  readonly editField: EditToolField;
}

/**
 * `ToolBaseToolsSection`'s two non-chip pieces — the MCP access toggle that
 * closes the Tools section, and the served group classification that decides
 * whether the chips inside it are grouped at all.
 *
 * Split into this sibling file for the same reason `IndexActionsParts.tsx`
 * records for its own split: `ToolBase.render.tsx` is at the repo's 400-line
 * budget (R-eslint(max-lines)). No behaviour change from the two functions
 * this used to hold inline.
 */
/**
 * The MCP access control, at the END of the Tools section (ELITEA-2687).
 *
 * WHAT CHANGED, AND WHAT DID NOT. It used to be a checkbox rendered BEFORE
 * the chips, labelled "Make tools available by MCP". It is now a switch
 * rendered AFTER every group, labelled "Enable MCP access for selected
 * tools" — because it governs the selection above it, and a control that
 * governs a list reads as belonging to that list only when it follows it.
 *
 * The STORED FIELD is untouched: `meta.mcp_options.available_by_mcp`, the
 * same boolean, written through the same `editField` path. A toolkit saved
 * before this change opens with the toggle already on, and a new toolkit
 * opens off because the key is simply absent — no migration, no default to
 * write (ELITEA-2693).
 *
 * It is a `BaseSwitch` rather than `ToolBaseProperty`'s boolean renderer for
 * exactly that reason: the property renderer draws a checkbox with the
 * schema `title` as its label, and there is no schema here — the field is
 * synthesised by this file, not served.
 */
export function resolveMcpExposureField(
  isMcpExposureEnabled: boolean,
  editToolDetail: EditToolDetail,
  passParams: McpExposurePassParams,
  disabled: boolean | undefined,
): ReactNode {
  if (!isMcpExposureEnabled) return null;
  const enabled = Boolean(editToolDetail.meta?.mcp_options?.['available_by_mcp']);
  return (
    <Box sx={mcpToggleSx}>
      <BaseSwitch
        checked={enabled}
        onChange={() => passParams.editField('meta.mcp_options.available_by_mcp', !enabled)}
        disabled={disabled}
        aria-label={t('features.toolkits.toolBase.mcpExposure.label', 'Enable MCP access for selected tools')}
        data-testid="toolkit-mcp-access-toggle"
      />
      <Typography variant="bodyMedium">{t('features.toolkits.toolBase.mcpExposure.label', 'Enable MCP access for selected tools')}</Typography>
      <InfoTooltip
        title={t(
          'features.toolkits.toolBase.mcpExposure.hint',
          'Publishes the tools selected above through this project\'s MCP server, so an external MCP client can call them with the toolkit\'s own credentials.',
        )}
        data-testid="toolkit-mcp-access-hint"
      />
    </Box>
  );
}

const mcpToggleSx = { display: 'flex', alignItems: 'center', gap: '0.5rem', marginTop: '1rem' };

/**
 * The served per-tool group classification, read off the same
 * `selected_tools` node the tool list itself comes from.
 *
 * Returns `undefined` — not an empty object — when the type carries none, so
 * `ToolActionsSelector` can tell "this toolkit's tools are not classified"
 * from "they are all in one group" and fall back to the flat list
 * (ELITEA-2688).
 */
export function resolveToolGroups(schema: ToolSchema): { readonly groups?: Readonly<Record<string, string>>; readonly order?: readonly string[] } {
  const selectedToolsSchema = schema.properties?.['selected_tools'];
  const groups = selectedToolsSchema?.tool_groups;
  const order = selectedToolsSchema?.tool_group_order;
  return {
    ...(groups !== undefined && Object.keys(groups).length > 0 ? { groups } : {}),
    ...(Array.isArray(order) && order.length > 0 ? { order } : {}),
  };
}

