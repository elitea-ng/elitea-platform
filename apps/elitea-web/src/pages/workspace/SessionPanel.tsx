/**
 * The thread's right-hand panel (toggle ⌘⌥\, resizable): what the thread's
 * turns changed, newest first — diff, Reveal in Finder / Open, and for a
 * turn the host still keeps per-file revert and undo — and the
 * approvals of this thread (the one being asked is a modal dialog; the panel
 * shows the queue behind it and what was decided).
 */
import { useState } from 'react';
import type { KeyboardEvent, PointerEvent } from 'react';

import CloseIcon from '@mui/icons-material/Close';
import FolderOpenOutlinedIcon from '@mui/icons-material/FolderOpenOutlined';
import LaunchOutlinedIcon from '@mui/icons-material/LaunchOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { ChangedFilesCard } from '@/features/workspace';
import type { AppIpc } from '@/shared/desktop/appEvents';
import type { ApprovalDecision, ApprovalRequestPayload, WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { CHANGES_WIDTH, TitleBarSpacer, useDesktopLayout } from '@/widgets/desktop-shell';

import type { ChangeSet } from './useThreadSession';

export interface DecidedApproval {
  requestId: string;
  title: string;
  decision: ApprovalDecision;
}

export interface SessionPanelProps {
  ipc: WorkspaceIpc;
  appIpc: AppIpc | undefined;
  workspaceId: string;
  /** Every turn of the thread that changed files, newest first. */
  changeSets: readonly ChangeSet[];
  /** The turn `/undo` asks to undo (the last one, when it changed files). */
  undoTurnId: string | null;
  undoRequest: number;
  waiting: readonly ApprovalRequestPayload[];
  decided: readonly DecidedApproval[];
}

function decisionLabel(decision: ApprovalDecision): string {
  switch (decision) {
    case 'allow_once':
      return t('workspace.panel.allowedOnce', 'Allowed once');
    case 'allow_always':
      return t('workspace.panel.allowedAlways', 'Always allowed');
    case 'deny':
      return t('workspace.panel.denied', 'Denied');
  }
}

function ResizeHandle(): React.JSX.Element {
  const width = useDesktopLayout((state) => state.changesWidth);
  const setWidth = useDesktopLayout((state) => state.setChangesWidth);
  const [drag, setDrag] = useState<{ x: number; width: number } | null>(null);
  const onPointerDown = (event: PointerEvent<HTMLDivElement>): void => {
    event.currentTarget.setPointerCapture(event.pointerId);
    setDrag({ x: event.clientX, width });
  };
  const onPointerMove = (event: PointerEvent<HTMLDivElement>): void => {
    // The panel is on the right: dragging left widens it.
    if (drag !== null) setWidth(drag.width + (drag.x - event.clientX));
  };
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>): void => {
    if (event.key === 'ArrowLeft') setWidth(width + 16);
    else if (event.key === 'ArrowRight') setWidth(width - 16);
  };
  return (
    <Box
      // oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- a focusable, draggable splitter (WAI-ARIA window splitter); an <hr> takes neither
      role="separator"
      aria-orientation="vertical"
      aria-label={t('workspace.panel.resize', 'Resize changes panel')}
      aria-valuemin={CHANGES_WIDTH.min}
      aria-valuemax={CHANGES_WIDTH.max}
      aria-valuenow={width}
      tabIndex={0}
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={() => setDrag(null)}
      onKeyDown={onKeyDown}
      sx={(theme: Theme) => ({
        width: '0.3125rem',
        marginRight: '-0.1875rem',
        flexShrink: 0,
        cursor: 'col-resize',
        position: 'relative',
        zIndex: 1,
        borderLeft: `1px solid ${theme.vars.palette.divider}`,
        '&:hover, &:focus-visible': { borderLeftColor: theme.vars.palette.primary.main, outline: 'none' },
      })}
    />
  );
}

function SectionTitle({ children }: { children: string }): React.JSX.Element {
  return (
    <Typography
      variant="labelSmall"
      component="h2"
      sx={(theme: Theme) => ({ margin: 0, color: theme.vars.palette.text.metrics, textTransform: 'uppercase', letterSpacing: '0.04em' })}
    >
      {children}
    </Typography>
  );
}

function PathActions({ appIpc, workspaceId, path }: { appIpc: AppIpc; workspaceId: string; path: string }): React.JSX.Element {
  const [failed, setFailed] = useState(false);
  const act = (kind: 'reveal' | 'open'): void => {
    const call = kind === 'reveal' ? appIpc.revealPath(workspaceId, path) : appIpc.openPath(workspaceId, path);
    call.then(
      () => setFailed(false),
      () => setFailed(true),
    );
  };
  return (
    <>
      <Tooltip title={failed ? t('workspace.panel.pathFailed', 'That did not work.') : t('workspace.panel.reveal', 'Reveal in Finder')}>
        <IconButton size="small" aria-label={t('workspace.panel.revealNamed', 'Reveal {{path}} in Finder', { path })} onClick={() => act('reveal')}>
          <FolderOpenOutlinedIcon fontSize="inherit" />
        </IconButton>
      </Tooltip>
      <Tooltip title={t('workspace.panel.open', 'Open')}>
        <IconButton size="small" aria-label={t('workspace.panel.openNamed', 'Open {{path}}', { path })} onClick={() => act('open')}>
          <LaunchOutlinedIcon fontSize="inherit" />
        </IconButton>
      </Tooltip>
    </>
  );
}

function TurnLabel({ text }: { text: string }): React.JSX.Element {
  return (
    <Typography
      variant="labelSmall"
      component="h3"
      title={text}
      sx={(theme: Theme) => ({ margin: 0, color: theme.vars.palette.text.secondary, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' })}
    >
      {text}
    </Typography>
  );
}

export function SessionPanel({ ipc, appIpc, workspaceId, changeSets, undoTurnId, undoRequest, waiting, decided }: SessionPanelProps): React.JSX.Element {
  const width = useDesktopLayout((state) => state.changesWidth);
  return (
    <>
      <ResizeHandle />
      <Box
        component="aside"
        data-testid="session-panel"
        aria-label={t('workspace.panel.label', 'Changes and approvals')}
        sx={(theme: Theme) => ({
          width: `${String(width / 16)}rem`,
          flexShrink: 0,
          display: 'flex',
          flexDirection: 'column',
          minHeight: 0,
          background: theme.vars.palette.background.default,
        })}
      >
        <Box sx={(theme: Theme) => ({ borderBottom: `1px solid ${theme.vars.palette.divider}` })}>
        <TitleBarSpacer
          logo={false}
          trailing={
            <Tooltip title={t('workspace.panel.hide', 'Hide panel')}>
              <IconButton size="small" aria-label={t('workspace.panel.hide', 'Hide panel')} onClick={() => useDesktopLayout.getState().setChangesOpen(false)}>
                <CloseIcon fontSize="inherit" />
              </IconButton>
            </Tooltip>
          }
        >
          <Typography variant="labelMedium" component="span">
            {t('workspace.panel.changes', 'Changes')}
          </Typography>
        </TitleBarSpacer>
        </Box>
        <Box sx={{ flex: 1, minHeight: 0, overflowY: 'auto', paddingX: 1.5, paddingY: 1.5, display: 'flex', flexDirection: 'column', gap: 2 }}>
          {changeSets.length === 0 ? (
            <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
              {t('workspace.panel.noChanges', 'No changes yet. The files the agent changes in this thread show up here.')}
            </Typography>
          ) : (
            changeSets.map((set) => (
              <Box key={set.turnId} component="section" data-testid="panel-turn-changes" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
                {changeSets.length > 1 && set.label !== '' && <TurnLabel text={set.label} />}
                <ChangedFilesCard
                  ipc={ipc}
                  turnId={set.turnId}
                  files={set.files}
                  undoRequest={set.turnId === undoTurnId ? undoRequest : 0}
                  variant="panel"
                  fileActions={appIpc === undefined ? undefined : (file) => <PathActions appIpc={appIpc} workspaceId={workspaceId} path={file.path} />}
                />
              </Box>
            ))
          )}
          {(waiting.length > 0 || decided.length > 0) && (
            <Box component="section" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }} data-testid="panel-approvals">
              <SectionTitle>{t('workspace.panel.approvals', 'Approvals')}</SectionTitle>
              <Box component="ul" sx={{ margin: 0, padding: 0, listStyle: 'none' }}>
                {waiting.map((request) => (
                  <Box component="li" key={request.request_id} sx={{ display: 'flex', gap: 1, paddingY: 0.25 }}>
                    <Typography variant="bodySmall" sx={{ flex: 1, minWidth: 0 }}>
                      {request.title}
                    </Typography>
                    <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.warning.main })}>
                      {t('workspace.panel.waiting', 'Waiting')}
                    </Typography>
                  </Box>
                ))}
                {decided.map((entry) => (
                  <Box component="li" key={entry.requestId} sx={{ display: 'flex', gap: 1, paddingY: 0.25 }}>
                    <Typography variant="bodySmall" sx={{ flex: 1, minWidth: 0 }}>
                      {entry.title}
                    </Typography>
                    <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
                      {decisionLabel(entry.decision)}
                    </Typography>
                  </Box>
                ))}
              </Box>
            </Box>
          )}
        </Box>
      </Box>
    </>
  );
}
