import { useMemo, type ComponentProps } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import type { ConfigurationTab } from '@/features/toolkits';

import { SHAREPOINT_AUTH_MODALS } from './sharepointAuthModals';
import type { McpLoadToolsSlot } from './useMcpLoadTools';

/**
 * The slot bag `EditToolkit.tsx` hands `ConfigurationTab`.
 *
 * `pages/` is the one layer allowed to compose `features/toolkits` with
 * `features/credentials` and `features/mcps`, so every one of these slots has to
 * be built here. It moved out of the page's JSX when the MCP "Load Tools" slot
 * joined the bag: the page sits at its §3.5 400-line ceiling, and a slot bag
 * that keeps growing does not belong inline in a render tree anyway.
 *
 * `credentialPickerSlots.tsx` is the same shape and the same reason.
 */
type ConfigurationTabSlots = ComponentProps<typeof ConfigurationTab>['slots'];

const testPaneSlotSx: SxProps<Theme> = { flex: 1, minWidth: 0 };

export interface UseConfigurationTabSlotsParams {
  readonly renderCredentialPicker: ConfigurationTabSlots['renderCredentialPicker'];
  /** Absent for a toolkit that is not MCP-shaped; see `./useMcpLoadTools.tsx`. */
  readonly mcpLoadTools: McpLoadToolsSlot | undefined;
}

export function useConfigurationTabSlots({ renderCredentialPicker, mcpLoadTools }: UseConfigurationTabSlotsParams): ConfigurationTabSlots {
  return useMemo(
    () => ({
      // The one place in the app that can legally hand SharePoint's
      // delegated-login UI a REAL `McpAuthModal` — see `./sharepointAuthModals.tsx`.
      sharepointAuth: SHAREPOINT_AUTH_MODALS,
      renderCredentialPicker,
      ...(mcpLoadTools !== undefined && { toolActionsExtra: mcpLoadTools }),
      // Composition gap: the right-pane live test-chat content (`TestTools`, a
      // sibling A4 sub-unit's owned file — see `ConfigurationTab.tsx`'s own
      // module doc comment for why this is a slot, not a direct import) has
      // real dependencies (`features/chat`, a `widgets/`-layer LLM model
      // selector) that do not exist anywhere in this worktree yet.
      renderTestPane: () => (
        <Box
          sx={testPaneSlotSx}
          data-testid="edit-toolkit-test-pane-slot"
        />
      ),
    }),
    [renderCredentialPicker, mcpLoadTools],
  );
}
