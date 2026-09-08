import { useMemo, type ComponentProps } from 'react';

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
      // `renderTestPane` is NOT supplied, and that is the fix rather than an
      // omission. It used to be supplied as an empty `<Box>` — the disclosed
      // composition gap that left the right-hand half of the toolkit editor
      // blank — on the grounds that the full `TestTools` surface needs
      // `features/chat` and a `widgets/`-layer model selector this app does not
      // have. `ConfigurationTab` now renders `features/toolkits`' own
      // `TestToolPane` when the slot is absent: pick a tool, fill its
      // arguments, Run, read the result, over the synchronous
      // `POST /elitea_core/test_tool/...` route. That pane imports nothing this
      // layer has to hand it, so the page supplies nothing.
    }),
    [renderCredentialPicker, mcpLoadTools],
  );
}
