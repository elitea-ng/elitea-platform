/**
 * The frame's inspector for the Elitea pages (`shared/desktop/inspectorSlot`):
 * a full-height pane at the window's right edge, on the sidebar's own
 * surface with a divider on its left, that a page fills through
 * `<InspectorPortal>` (the chat puts its participants, agents and context
 * budget there). It shows while a page has one and the inspector is open;
 * the open state is the workspace inspector's (`changesOpen`, ⌘⌥\, kept per
 * viewer), so one toggle and one shortcut serve every page.
 */
import { useCallback, useMemo, useState, type ReactNode } from 'react';

import CloseIcon from '@mui/icons-material/Close';
import VerticalSplitOutlinedIcon from '@mui/icons-material/VerticalSplitOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { InspectorSlotProvider, type InspectorKind, type InspectorSlot } from '@/shared/desktop/inspectorSlot';
import { t } from '@/shared/i18n';

import { SIDEBAR_TINT_OPACITY } from '../lib/sidebarTint';
import { useDesktopLayout } from '../model/desktopLayout.store';
import { modKey } from './shortcutLabel';
import { TitleBarSpacer } from './TitleBarSpacer';

/** The pane's width: the participants rail's own (276px), rounded up to the 8px grid. */
const INSPECTOR_WIDTH_REM = 17.5;

export interface InspectorHost {
  readonly slot: InspectorSlot;
  /** The title the page on screen gave its inspector; `null` when it has none. */
  readonly title: string | null;
  readonly setTarget: (node: HTMLElement | null) => void;
}

let nextRegistration = 0;

const TITLES: Record<InspectorKind, () => string> = {
  chat: () => t('desktop.inspector.chat', 'Conversation'),
};

/** The frame's side of the slot: which page has an inspector, and the pane's body to portal into. */
export function useInspectorHost(): InspectorHost {
  const [target, setTarget] = useState<HTMLElement | null>(null);
  const [kinds, setKinds] = useState<readonly { readonly id: number; readonly kind: InspectorKind }[]>([]);
  const register = useCallback((kind: InspectorKind) => {
    nextRegistration += 1;
    const id = nextRegistration;
    setKinds((current) => [...current, { id, kind }]);
    return () => setKinds((current) => current.filter((entry) => entry.id !== id));
  }, []);
  const slot = useMemo(() => ({ target, register }), [target, register]);
  const kind = kinds.at(-1)?.kind;
  return { slot, title: kind === undefined ? null : TITLES[kind](), setTarget };
}

export function InspectorSlotScope({ host, children }: { readonly host: InspectorHost; readonly children: ReactNode }): React.JSX.Element {
  return <InspectorSlotProvider value={host.slot}>{children}</InspectorSlotProvider>;
}

/** The title bar's toggle: shown while the page has an inspector and it is hidden. */
export function InspectorToggle({ host }: { readonly host: InspectorHost }): React.JSX.Element | null {
  const open = useDesktopLayout((state) => state.changesOpen);
  if (host.title === null || open) return null;
  const label = t('desktop.inspector.show', 'Show inspector');
  return (
    <Tooltip title={`${label} (${modKey()}⌥\\)`}>
      <IconButton size="small" aria-label={label} aria-pressed={false} data-testid="inspector-toggle" onClick={() => useDesktopLayout.getState().setChangesOpen(true)}>
        <VerticalSplitOutlinedIcon fontSize="inherit" />
      </IconButton>
    </Tooltip>
  );
}

export function InspectorPane({ host }: { readonly host: InspectorHost }): React.JSX.Element | null {
  const open = useDesktopLayout((state) => state.changesOpen);
  const vibrancy = useDesktopLayout((state) => state.platform.vibrancy);
  if (host.title === null || !open) return null;
  const hide = t('desktop.inspector.hide', 'Hide inspector');
  return (
    <Box
      component="aside"
      data-chrome
      data-testid="desktop-inspector"
      aria-label={host.title}
      sx={(theme: Theme) => ({
        width: `${String(INSPECTOR_WIDTH_REM)}rem`,
        flexShrink: 0,
        height: '100vh',
        display: 'flex',
        flexDirection: 'column',
        boxSizing: 'border-box',
        borderLeft: `1px solid ${theme.vars.palette.divider}`,
        position: 'relative',
        isolation: 'isolate',
        // The sidebar's surface (`DesktopSidebar.tsx`): the secondary background, tinted over the window's material.
        '&::before': {
          content: '""',
          position: 'absolute',
          inset: 0,
          zIndex: -1,
          background: theme.vars.palette.background.secondary,
          opacity: vibrancy ? SIDEBAR_TINT_OPACITY : 1,
        },
      })}
    >
      <TitleBarSpacer
        logo={false}
        trailing={
          <Tooltip title={`${hide} (${modKey()}⌥\\)`}>
            <IconButton size="small" aria-label={hide} onClick={() => useDesktopLayout.getState().setChangesOpen(false)}>
              <CloseIcon fontSize="inherit" />
            </IconButton>
          </Tooltip>
        }
      >
        <Typography variant="labelSmall" component="h2" sx={(theme: Theme) => ({ margin: 0, fontWeight: 600, color: theme.vars.palette.text.metrics })}>
          {host.title}
        </Typography>
      </TitleBarSpacer>
      <Box ref={host.setTarget} data-testid="desktop-inspector-body" sx={{ flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column' }} />
    </Box>
  );
}
