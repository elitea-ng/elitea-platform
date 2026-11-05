/**
 * The browser pane's body (ADR-0048 decision 1): a toolbar — address field,
 * back, forward, reload or stop, the control pill's place (#1591), Open in
 * browser, a ⋯ menu (Forget site data) and close — above the page
 * rectangle. The page itself is a native child webview the host lays over
 * that rectangle (`usePageRect`): nothing can be drawn over it, so every
 * control, menu and notice of the pane sits outside it, and the page is
 * hidden while a modal sheet is open.
 */
import { useEffect, useRef, useState, type MouseEvent } from 'react';

import ArrowBackIcon from '@mui/icons-material/ArrowBack';
import ArrowForwardIcon from '@mui/icons-material/ArrowForward';
import CloseIcon from '@mui/icons-material/Close';
import CloseOutlinedIcon from '@mui/icons-material/CloseOutlined';
import MoreHorizIcon from '@mui/icons-material/MoreHoriz';
import OpenInNewIcon from '@mui/icons-material/OpenInNew';
import RefreshIcon from '@mui/icons-material/Refresh';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import InputBase from '@mui/material/InputBase';
import Menu from '@mui/material/Menu';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import type { BrowserGoAction, BrowserIpc } from '@/shared/desktop/browserIpc';
import { toWorkspaceIpcError } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { MenuRow, MenuRowText } from '@/shared/ui/native-menu';

import { useBrowserPane, useBrowserPanes } from '../model/browserPane.store';
import { useBrowserHost } from '../model/ipcContext';
import { usePageRect, type PageRectReport } from '../model/usePageRect';

export interface BrowserPaneProps {
  /** The thread's conversation id: the page and its site data are this thread's. */
  readonly threadId: string;
  /** The pane closes (× in its toolbar): the inspector drops the tab. */
  readonly onClose: () => void;
}

/** Only http and https are handed to the system browser. */
function externalUrl(raw: string): string | undefined {
  try {
    const url = new URL(raw);
    return url.protocol === 'https:' || url.protocol === 'http:' ? url.href : undefined;
  } catch {
    return undefined;
  }
}

function ToolButton({ label, onClick, disabled = false, testId, children }: { readonly label: string; readonly onClick: (event: MouseEvent<HTMLElement>) => void; readonly disabled?: boolean; readonly testId: string; readonly children: React.ReactNode }): React.JSX.Element {
  return (
    <Tooltip title={label}>
      <span>
        <IconButton size="small" aria-label={label} onClick={onClick} disabled={disabled} data-testid={testId}>
          {children}
        </IconButton>
      </span>
    </Tooltip>
  );
}

/** The control pill's place (Agent / You / Idle, #1591): reserved so the toolbar does not move when it lands. */
function PillSlot(): React.JSX.Element {
  return <Box data-testid="browser-pill-slot" aria-hidden sx={{ width: '3.5rem', flexShrink: 0 }} />;
}

function Toolbar({ ipc, threadId, onClose, onError, onNavigate }: { readonly ipc: BrowserIpc; readonly threadId: string; readonly onClose: () => void; readonly onError: (message: string | null) => void; readonly onNavigate: (address: string) => void }): React.JSX.Element {
  const pane = useBrowserPane(threadId);
  const { openExternal } = useBrowserHost();
  const [address, setAddress] = useState(pane.url);
  const [editing, setEditing] = useState(false);
  const [menu, setMenu] = useState<HTMLElement | null>(null);
  // The field follows the page unless the person is typing in it.
  useEffect(() => {
    if (!editing) setAddress(pane.url);
  }, [pane.url, editing]);

  const go = (action: BrowserGoAction): void => {
    ipc.go(threadId, action).then(
      () => onError(null),
      (error: unknown) => onError(toWorkspaceIpcError(error).message),
    );
  };
  const submit = (): void => {
    setEditing(false);
    const target = address.trim();
    if (target !== '') onNavigate(target);
  };
  const external = externalUrl(pane.url);
  const forget = (): void => {
    setMenu(null);
    ipc.forget(threadId).then(
      () => {
        useBrowserPanes.getState().reset(threadId);
        onClose();
      },
      (error: unknown) => onError(toWorkspaceIpcError(error).message),
    );
  };

  return (
    <Box
      component="form"
      onSubmit={(event) => {
        event.preventDefault();
        submit();
      }}
      data-testid="browser-toolbar"
      sx={(theme: Theme) => ({ display: 'flex', alignItems: 'center', gap: 0.25, paddingX: 0.5, paddingY: 0.5, borderBottom: `1px solid ${theme.vars.palette.divider}`, flexShrink: 0 })}
    >
      <ToolButton label={t('workspace.browser.back', 'Back')} onClick={() => go('back')} disabled={!pane.live} testId="browser-back">
        <ArrowBackIcon fontSize="inherit" />
      </ToolButton>
      <ToolButton label={t('workspace.browser.forward', 'Forward')} onClick={() => go('forward')} disabled={!pane.live} testId="browser-forward">
        <ArrowForwardIcon fontSize="inherit" />
      </ToolButton>
      {pane.loading ? (
        <ToolButton label={t('workspace.browser.stop', 'Stop')} onClick={() => go('stop')} testId="browser-stop">
          <CloseIcon fontSize="inherit" />
        </ToolButton>
      ) : (
        <ToolButton label={t('workspace.browser.reload', 'Reload')} onClick={() => go('reload')} disabled={!pane.live} testId="browser-reload">
          <RefreshIcon fontSize="inherit" />
        </ToolButton>
      )}
      <InputBase
        value={address}
        onChange={(event) => {
          setEditing(true);
          setAddress(event.target.value);
        }}
        onBlur={() => setEditing(false)}
        onKeyDown={(event) => {
          if (event.key === 'Escape') {
            setEditing(false);
            setAddress(pane.url);
          }
        }}
        placeholder={t('workspace.browser.addressPlaceholder', 'Enter a web address')}
        inputProps={{ 'aria-label': t('workspace.browser.address', 'Address'), spellCheck: false, autoCapitalize: 'off', autoCorrect: 'off', 'data-testid': 'browser-address' }}
        sx={(theme: Theme) => ({ flex: 1, minWidth: 0, fontSize: '0.8125rem', paddingX: 1, height: '1.75rem', borderRadius: '6px', background: theme.vars.palette.background.default })}
      />
      <PillSlot />
      <ToolButton label={t('workspace.browser.openExternal', 'Open in browser')} onClick={() => void (external === undefined ? undefined : openExternal?.(external).catch(() => undefined))} disabled={external === undefined || openExternal === undefined} testId="browser-open-external">
        <OpenInNewIcon fontSize="inherit" />
      </ToolButton>
      <ToolButton label={t('workspace.browser.more', 'More')} onClick={(event) => setMenu(event.currentTarget)} testId="browser-more">
        <MoreHorizIcon fontSize="inherit" />
      </ToolButton>
      <ToolButton
        label={t('workspace.browser.close', 'Close browser')}
        onClick={() => {
          void ipc.close(threadId).catch(() => undefined);
          useBrowserPanes.getState().update(threadId, { open: false, live: false, loading: false });
          onClose();
        }}
        testId="browser-close"
      >
        <CloseOutlinedIcon fontSize="inherit" />
      </ToolButton>
      <Menu anchorEl={menu} open={menu !== null} onClose={() => setMenu(null)} slotProps={{ list: { dense: true } }}>
        <MenuRow onClick={forget} data-testid="browser-forget">
          <MenuRowText>{t('workspace.browser.forget', 'Forget site data')}</MenuRowText>
        </MenuRow>
      </Menu>
    </Box>
  );
}

/** Notices sit above the page, never on it. */
function Notice({ children, testId }: { readonly children: React.ReactNode; readonly testId: string }): React.JSX.Element {
  return (
    <Typography variant="bodySmall" component="output" data-testid={testId} sx={(theme: Theme) => ({ paddingX: 1.5, paddingY: 0.75, color: theme.vars.palette.text.metrics, borderBottom: `1px solid ${theme.vars.palette.divider}`, flexShrink: 0 })}>
      {children}
    </Typography>
  );
}

function Pane({ ipc, threadId, onClose }: BrowserPaneProps & { readonly ipc: BrowserIpc }): React.JSX.Element {
  const pane = useBrowserPane(threadId);
  const [error, setError] = useState<string | null>(null);
  // The latest rectangle, kept for the next open; once the page is live the host hears every change.
  const rect = useRef<PageRectReport | null>(null);
  const live = useRef(pane.live);
  useEffect(() => {
    live.current = pane.live;
  });
  const pageRef = usePageRect((next) => {
    rect.current = next;
    if (live.current) void ipc.setRect(threadId, next.rect, next.visible).catch(() => undefined);
  });

  // An address typed in the toolbar opens (or re-points) the page over the rectangle.
  const navigate = (address: string): void => {
    const at = rect.current;
    if (at === null) return;
    ipc.open(threadId, address, at.rect).then(
      ({ url }) => {
        setError(null);
        live.current = true;
        useBrowserPanes.getState().update(threadId, { open: true, live: true, url, loading: true, refused: null });
        // A modal came up (or the box went away) meanwhile: the new page is hidden too.
        const now = rect.current ?? at;
        if (!now.visible) void ipc.setRect(threadId, now.rect, false).catch(() => undefined);
      },
      (failure: unknown) => setError(toWorkspaceIpcError(failure).message),
    );
  };

  return (
    <Box data-testid="browser-pane" sx={{ flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column' }}>
      <Toolbar ipc={ipc} threadId={threadId} onClose={onClose} onError={setError} onNavigate={navigate} />
      {error !== null && <Notice testId="browser-error">{error}</Notice>}
      {pane.refused !== null && <Notice testId="browser-refused">{pane.refused.message}</Notice>}
      <Box ref={pageRef} data-testid="browser-page" sx={{ flex: 1, minHeight: 0, position: 'relative' }}>
        {!pane.live && (
          <Box sx={{ position: 'absolute', inset: 0, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 3, textAlign: 'center' }}>
            <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })} data-testid="browser-empty">
              {t('workspace.browser.empty', 'Type an address to open a page. Its cookies and site data stay with this thread.')}
            </Typography>
          </Box>
        )}
      </Box>
    </Box>
  );
}

/** The pane, or nothing without a host (a plain browser tab). */
export function BrowserPane(props: BrowserPaneProps): React.JSX.Element | null {
  const { ipc } = useBrowserHost();
  if (ipc === undefined) return null;
  return <Pane ipc={ipc} {...props} />;
}
