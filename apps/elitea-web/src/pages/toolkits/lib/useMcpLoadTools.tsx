import { useCallback, useMemo, useState, type ReactNode } from 'react';

import Typography from '@mui/material/Typography';

import { McpAuthModal, useGetRemoteMcpTools } from '@/features/mcps';
import { t } from '@/shared/i18n';

import type { EditToolDetail } from './toolkitFormTypes';

/**
 * "Load Tools" for an MCP toolkit — the composition root that fills
 * `ToolBaseSlots.toolActionsExtra`.
 *
 * WHY THIS FILE EXISTS. `features/toolkits/ui/form/ToolBase/
 * ToolActionsSelector.tsx` has rendered a "Load Tools" action for a Remote MCP
 * or pre-built MCP toolkit since the port, driven entirely by four
 * caller-injected props (`onLoadTools`, `isLoadingTools`, `canLoadTools`,
 * `mcpAuthModal`). Its own doc comment explains why they are injected: the
 * baseline's fetch hook is `features/mcps`' `useGetRemoteMcpTools`, and
 * `no-sideways-features` forbids `features/toolkits` to import it. The slot was
 * correct, and NO caller anywhere in `src/` ever supplied it — so the action
 * rendered permanently disabled, a click did nothing, and no MCP toolkit's
 * tools could be loaded from the browser. `pages/` is the layer allowed to
 * compose both slices, so the wiring belongs here. This is the same shape, and
 * the same defect, `./sharepointAuthModals.tsx` records for SharePoint's login
 * modal.
 *
 * NOTHING IS REIMPLEMENTED HERE. `useGetRemoteMcpTools` already does the whole
 * job — the `mcp_sync_tools` request, the socket id, the stored OAuth tokens,
 * the pre-built/remote split, and the retry once authorization completes. This
 * file supplies its inputs, renders its auth modal, and writes its result into
 * the form.
 *
 * WHERE THE RESULT GOES. `settings.available_mcp_tools` — the exact key
 * `ToolBase.render.tsx`'s `resolveAvailableTools` reads for a type that
 * declares no `args_schemas`. A remote server's tools are discovered, never
 * declared, so that key is the only source the chip picker has.
 *
 * WHY A PRE-BUILT TYPE IS REGISTERED AND A REMOTE ONE IS NOT. That asymmetry
 * belongs to the hook and to the SDK, not to this file, and it is correct:
 * `mcp_sync_tools` stores a discovery under `toolkit_type` (see
 * `MCPSyncTools` in elitea-main), and the SDK's static-registry reader
 * SKIPS `type == 'mcp'` outright — `elitea_sdk/runtime/toolkits/tools.py`'s
 * `_mcp_tools` — because a generic Remote MCP is resolved by dynamic
 * discovery from its own stored `settings.url` at run time. Registering one
 * would write a row nothing reads, under a name every other Remote MCP in the
 * project would collide with.
 */

/** The four props `ToolBaseSlots.toolActionsExtra` accepts. */
export interface McpLoadToolsSlot {
  readonly onLoadTools: () => void;
  readonly isLoadingTools: boolean;
  readonly canLoadTools: boolean;
  readonly mcpAuthModal: ReactNode;
}

/** Module-private: `UseMcpLoadToolsParams` is its only user, and the knip dead-code gate counts an unused export as dead code — correctly, since exporting it would advertise a seam nothing uses. */
type ApplyToolDetail = (updater: (previous: EditToolDetail | null) => EditToolDetail | null) => void;

/**
 * The same two rules `ToolBase.render.tsx` uses to decide whether the action is
 * rendered at all: `schema.title === 'mcp'` for the generic Remote MCP type,
 * and an `mcp_`-prefixed type for a pre-built server. Keyed on the toolkit TYPE
 * here, which is the same string the served schema's `title` carries.
 */
function isMcpToolkitType(toolkitType: string | undefined): boolean {
  if (toolkitType === undefined || toolkitType === '') return false;
  return toolkitType === 'mcp' || toolkitType.startsWith('mcp_');
}

/** A pre-built type needs no URL of its own: the catalogue row supplies it server-side. */
function hasSomethingToDial(toolkitType: string, settings: Readonly<Record<string, unknown>>): boolean {
  if (toolkitType.startsWith('mcp_') && toolkitType !== 'mcp') return true;
  const url = settings['url'];
  return typeof url === 'string' && url.trim() !== '';
}

/** The tool NAMES out of `mcp_sync_tools`' loosely typed tool list. A nameless entry is dropped: a blank chip is unselectable. */
function toolNamesOf(tools: readonly unknown[]): readonly string[] {
  return tools
    .map((tool) => (typeof tool === 'object' && tool !== null ? (tool as { name?: unknown }).name : undefined))
    .filter((name): name is string => typeof name === 'string' && name !== '');
}

/**
 * The `values` shape `useGetRemoteMcpTools` reads: the toolkit type plus the
 * settings it sends. `settings` is always present, never `undefined` —
 * `exactOptionalPropertyTypes` makes the difference load-bearing at the call
 * site, and an always-built object is simpler than widening the hook's type.
 */
interface RemoteMcpValues {
  readonly type: string | undefined;
  readonly settings: { url?: string; headers?: Record<string, string>; timeout?: number; ssl_verify?: boolean };
}

function toRemoteMcpValues(editToolDetail: EditToolDetail | null | undefined): RemoteMcpValues {
  const settings = editToolDetail?.settings ?? {};
  const url = settings['url'];
  const headers = settings['headers'];
  const timeout = settings['timeout'];
  return {
    type: editToolDetail?.type,
    settings: {
      ...(typeof url === 'string' ? { url } : {}),
      ...(typeof headers === 'object' && headers !== null ? { headers: headers as Record<string, string> } : {}),
      // The form stores a number, but the baseline's own schema types this
      // field as `int | str` because the UI has been known to send a string.
      ...(typeof timeout === 'number' ? { timeout } : {}),
      ...(typeof timeout === 'string' && timeout.trim() !== '' && Number.isFinite(Number(timeout)) ? { timeout: Number(timeout) } : {}),
    },
  };
}

export interface UseMcpLoadToolsParams {
  readonly projectId: string | undefined;
  readonly editToolDetail: EditToolDetail | null | undefined;
  readonly onChangeToolDetail: ApplyToolDetail;
}

/**
 * Returns the slot for an MCP toolkit, and `undefined` for every other type so
 * a non-MCP form is untouched.
 */
export function useMcpLoadTools({ projectId, editToolDetail, onChangeToolDetail }: UseMcpLoadToolsParams): McpLoadToolsSlot | undefined {
  const [error, setError] = useState<string | undefined>(undefined);

  const toolkitType = editToolDetail?.type;
  /** Memoized: a fresh object every render would churn the hook below through every one of its own memos. */
  const values = useMemo(() => toRemoteMcpValues(editToolDetail), [editToolDetail]);

  const onToolsFetched = useCallback(
    (tools: readonly unknown[]) => {
      setError(undefined);
      const names = toolNamesOf(tools);
      onChangeToolDetail((previous) =>
        previous === null ? previous : { ...previous, settings: { ...previous.settings, available_mcp_tools: names } },
      );
    },
    [onChangeToolDetail],
  );

  // A failed discovery answers HTTP 200 with `success: false`, so it must be
  // rendered as its own state. Writing an empty tool list instead would report
  // a dead server as a server that publishes no tools.
  const onError = useCallback((message: string) => setError(message), []);

  const { fetchTools, isLoading, getModalProps } = useGetRemoteMcpTools({
    values,
    ...(toolkitType !== undefined ? { toolkitType } : {}),
    ...(projectId !== undefined ? { projectId } : {}),
    onToolsFetched,
    onError,
  });

  const canLoadTools = useMemo(
    () => projectId !== undefined && isMcpToolkitType(toolkitType) && hasSomethingToDial(toolkitType ?? '', editToolDetail?.settings ?? {}),
    [projectId, toolkitType, editToolDetail?.settings],
  );

  /**
   * The slot renders inside the tool section, and it carries BOTH the real
   * OAuth modal and the failure message. `ToolActionsSelector` has no error
   * slot of its own, and a silent failure reads as "this server has no tools".
   */
  const mcpAuthModal = useMemo<ReactNode>(
    () => (
      <>
        <McpAuthModal {...getModalProps()} />
        {error !== undefined && (
          <Typography
            role="alert"
            variant="bodySmall"
            color="error"
          >
            {t('pages.toolkits.mcpLoadTools.error', 'Could not load the tools of this MCP server: ')}
            {error}
          </Typography>
        )}
      </>
    ),
    [getModalProps, error],
  );

  if (!isMcpToolkitType(toolkitType)) return undefined;
  return { onLoadTools: fetchTools, isLoadingTools: isLoading, canLoadTools, mcpAuthModal };
}
