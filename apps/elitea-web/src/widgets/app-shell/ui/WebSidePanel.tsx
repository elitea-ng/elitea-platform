/**
 * The web shell's right side panel (`model/useWebSidePanelHost.ts`): the
 * tabs the page on screen offers — a list's Filters, the chat's Conversation —
 * as chips, the active tab's content below them, and a close button. The
 * browser's counterpart of the desktop frame's inspector, with the same
 * structure: everything that belongs beside a page lives here, not floating
 * over it.
 *
 * A flex sibling of `<main>` on a wide window; a temporary drawer below
 * 1024px. Nothing at all while the page offers no tab.
 */
import type { ReactNode } from 'react';

import CloseIcon from '@mui/icons-material/Close';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Drawer from '@mui/material/Drawer';
import IconButton from '@mui/material/IconButton';
import type { SxProps, Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';
import { useAnchoredTooltip } from '@/shared/ui/lib/useAnchoredTooltip';

import type { WebSidePanelHost } from '../model/useWebSidePanelHost';

const WEB_SIDE_PANEL_WIDTH_PX = 320;

function PanelContent({ host }: { readonly host: WebSidePanelHost }): ReactNode {
  const closeLabel = t('shell.sidePanel.close', 'Close panel');
  // The panel opens beside a pointer that never entered this button; its
  // tooltip then sat over the panel's own first controls.
  const closeTip = useAnchoredTooltip<HTMLButtonElement>();
  return (
    <>
      <Box sx={headerSx}>
        <Box
          role="tablist"
          aria-label={t('shell.sidePanel.label', 'Side panel')}
          sx={chipsSx}
        >
          {host.tabs.map((info) => {
            const selected = info.id === host.active;
            return (
              <ButtonBase
                key={info.id}
                role="tab"
                aria-selected={selected}
                aria-controls="web-side-panel-body"
                data-testid={`side-panel-chip-${info.id}`}
                onClick={() => host.activate(info.id)}
                sx={chipSx(selected)}
              >
                {info.title}
              </ButtonBase>
            );
          })}
        </Box>
        <Tooltip title={closeLabel} {...closeTip.tooltipProps}>
          <IconButton
            ref={closeTip.anchorRef}
            size="small"
            aria-label={closeLabel}
            data-testid="side-panel-close"
            onClick={() => {
              closeTip.close();
              host.setOpen(false);
            }}
          >
            <CloseIcon fontSize="inherit" />
          </IconButton>
        </Tooltip>
      </Box>
      <Box
        id="web-side-panel-body"
        role="tabpanel"
        ref={host.setBody}
        data-testid={host.active === null ? undefined : `side-panel-body-${host.active}`}
        sx={bodySx}
      />
    </>
  );
}

export function WebSidePanel({ host }: { readonly host: WebSidePanelHost }): ReactNode {
  if (host.narrow) {
    return (
      <Drawer
        anchor="right"
        open={host.shown}
        onClose={() => host.setOpen(false)}
        slotProps={{ paper: { sx: drawerPaperSx } }}
      >
        <Box
          data-testid="web-side-panel"
          sx={drawerColumnSx}
        >
          <PanelContent host={host} />
        </Box>
      </Drawer>
    );
  }
  if (!host.shown) return null;
  return (
    <Box
      component="aside"
      data-testid="web-side-panel"
      aria-label={t('shell.sidePanel.label', 'Side panel')}
      sx={asideSx}
    >
      <PanelContent host={host} />
    </Box>
  );
}

const asideSx: SxProps<Theme> = (theme: Theme) => ({
  width: `${String(WEB_SIDE_PANEL_WIDTH_PX)}px`,
  flexShrink: 0,
  height: '100vh',
  boxSizing: 'border-box',
  display: 'flex',
  flexDirection: 'column',
  borderLeft: `1px solid ${theme.vars.palette.divider}`,
  background: theme.vars.palette.background.secondary,
});

const drawerPaperSx: SxProps<Theme> = (theme: Theme) => ({
  width: `min(${String(WEB_SIDE_PANEL_WIDTH_PX)}px, 100vw)`,
  display: 'flex',
  flexDirection: 'column',
  background: theme.vars.palette.background.secondary,
});

const drawerColumnSx: SxProps<Theme> = { height: '100%', display: 'flex', flexDirection: 'column' };

const headerSx: SxProps<Theme> = (theme: Theme) => ({
  flexShrink: 0,
  minHeight: '2.75rem',
  boxSizing: 'border-box',
  display: 'flex',
  alignItems: 'center',
  gap: 1,
  paddingInline: theme.spacing(1.5, 1),
  borderBottom: `1px solid ${theme.vars.palette.divider}`,
});

const chipsSx: SxProps<Theme> = { flex: 1, minWidth: 0, display: 'flex', gap: 0.5, overflowX: 'auto', scrollbarWidth: 'none' };

const chipSx =
  (selected: boolean): SxProps<Theme> =>
  (theme: Theme) => ({
    height: '1.5rem',
    paddingInline: 1,
    borderRadius: theme.vars.shape.radiusSm,
    typography: 'labelSmall',
    whiteSpace: 'nowrap',
    color: selected ? theme.vars.palette.text.secondary : theme.vars.palette.text.metrics,
    background: selected ? theme.vars.palette.background.button.secondary.hover : 'transparent',
    '&:hover': { color: theme.vars.palette.text.secondary },
    '&.Mui-focusVisible': { outline: `2px solid ${theme.vars.palette.primary.main}` },
  });

const bodySx: SxProps<Theme> = { flex: 1, minHeight: 0, overflowY: 'auto', display: 'flex', flexDirection: 'column' };
