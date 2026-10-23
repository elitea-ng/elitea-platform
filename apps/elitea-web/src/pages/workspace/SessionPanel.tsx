/**
 * The thread's tabs in the frame's inspector (toggle ⌘⌥\): Changes — what
 * the thread's turns changed, newest first: diff, Reveal in Finder / Open,
 * and for a turn the host still keeps per-file revert and undo; Approvals —
 * the approvals of this thread (the one being asked is a modal dialog; the
 * tab shows the queue behind it and what was decided); Plan — the agent's
 * checklist (offered once it keeps one, or in plan mode); Processes — the
 * folder's background processes and their terminals (offered once it has
 * one); Index — the folder's local code index.
 */
import { Suspense, useState } from 'react';

import FolderOpenOutlinedIcon from '@mui/icons-material/FolderOpenOutlined';
import LaunchOutlinedIcon from '@mui/icons-material/LaunchOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { ChangedFilesCard, plan } from '@/features/workspace';
import type { AppIpc } from '@/shared/desktop/appEvents';
import { InspectorTab } from '@/shared/desktop/inspectorSlot';
import type { PlanView } from '@/shared/desktop/planIpc';
import type { ApprovalDecision, ApprovalRequestPayload, WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { IndexPanelHost } from '@/features/workspace-index';
import { copyText, useContextMenu } from '@/widgets/desktop-shell';

import type { ChangeSet, DecidedApproval } from './sessionTypes';
import { useHasProcesses } from './useHasProcesses';

export interface SessionPanelProps {
  ipc: WorkspaceIpc;
  appIpc: AppIpc | undefined;
  workspaceId: string;
  /** What the tabs show (the thread's title, or the folder's name): a tab's own window is named after it. */
  subject: string;
  /** The host has a local index to show in the Index tab. */
  hasIndex: boolean;
  /** Every turn of the thread that changed files, newest first. */
  changeSets: readonly ChangeSet[];
  /** The turn `/undo` asks to undo (the last one, when it changed files). */
  undoTurnId: string | null;
  undoRequest: number;
  waiting: readonly ApprovalRequestPayload[];
  decided: readonly DecidedApproval[];
  /** The agent's checklist (the Plan tab); `null` when it keeps none. */
  plan: PlanView | null;
  /** The thread is in plan mode: the Plan tab is offered even before a plan exists. */
  planMode: boolean;
}

function decisionLabel(decision: ApprovalDecision): string {
  switch (decision) {
    case 'allow_once':
      return t('workspace.panel.allowedOnce', 'Allowed once');
    case 'allow_always':
      return t('workspace.panel.allowedAlways', 'Always allowed');
    case 'deny':
      return t('workspace.panel.denied', 'Denied');
    case 'deny_with_feedback':
      return t('workspace.panel.deniedWithFeedback', 'Denied, with a reason');
    case 'accept':
      return t('workspace.panel.accepted', 'Accepted');
    case 'decline':
      return t('workspace.panel.declined', 'Declined');
    case 'continue':
      return t('workspace.panel.continued', 'Continued');
    case 'cancel':
      return t('workspace.panel.cancelled', 'Cancelled');
  }
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

function ChangesPane({ ipc, appIpc, workspaceId, changeSets, undoTurnId, undoRequest }: Pick<SessionPanelProps, 'ipc' | 'appIpc' | 'workspaceId' | 'changeSets' | 'undoTurnId' | 'undoRequest'>): React.JSX.Element {
  const showMenu = useContextMenu();
  const fileMenu = (file: { path: string }, event: React.MouseEvent): void =>
    showMenu(event, [
      { id: 'reveal', label: t('workspace.panel.reveal', 'Reveal in Finder'), enabled: appIpc !== undefined, run: () => void appIpc?.revealPath(workspaceId, file.path).catch(() => undefined) },
      { id: 'open', label: t('workspace.panel.open', 'Open'), enabled: appIpc !== undefined, run: () => void appIpc?.openPath(workspaceId, file.path).catch(() => undefined) },
      { separator: true },
      { id: 'copy', label: t('workspace.path.copy', 'Copy Path'), run: () => copyText(file.path) },
    ]);
  if (changeSets.length === 0) {
    return (
      <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
        {t('workspace.panel.noChanges', 'No changes yet. The files the agent changes in this thread show up here.')}
      </Typography>
    );
  }
  return (
    <>
      {changeSets.map((set) => (
        <Box key={set.turnId} component="section" data-testid="panel-turn-changes" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
          {changeSets.length > 1 && set.label !== '' && <TurnLabel text={set.label} />}
          <ChangedFilesCard
            ipc={ipc}
            turnId={set.turnId}
            files={set.files}
            undoRequest={set.turnId === undoTurnId ? undoRequest : 0}
            variant="panel"
            onFileContextMenu={fileMenu}
            fileActions={appIpc === undefined ? undefined : (file) => <PathActions appIpc={appIpc} workspaceId={workspaceId} path={file.path} />}
          />
        </Box>
      ))}
    </>
  );
}

function ApprovalsPane({ waiting, decided }: Pick<SessionPanelProps, 'waiting' | 'decided'>): React.JSX.Element {
  if (waiting.length === 0 && decided.length === 0) {
    return (
      <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
        {t('workspace.panel.noApprovals', 'Nothing was asked of you in this thread yet.')}
      </Typography>
    );
  }
  return (
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
  );
}

/** A tab's body: scrolls on its own, padded like the rest of the inspector. */
function TabBody({ children }: { children: React.ReactNode }): React.JSX.Element {
  return <Box sx={{ flex: 1, minHeight: 0, overflowY: 'auto', paddingX: 1.5, paddingY: 1.5, display: 'flex', flexDirection: 'column', gap: 2 }}>{children}</Box>;
}

/**
 * The thread's tabs in the frame's inspector: Changes, Approvals (its chip
 * counts the approvals waiting), Plan and Processes when there is one to
 * show and, when the host keeps a local index, Index. Each is a tab of its own, so it can share a group with the others,
 * sit in the other group, or open in a window of its own.
 */
export function SessionInspector(props: SessionPanelProps): React.JSX.Element {
  const { subject } = props;
  const hasProcesses = useHasProcesses(props.workspaceId);
  return (
    <>
      <InspectorTab id="changes" title={t('workspace.panel.changes', 'Changes')} subject={subject}>
        <TabBody>
          <ChangesPane {...props} />
        </TabBody>
      </InspectorTab>
      <InspectorTab id="approvals" title={t('workspace.panel.approvals', 'Approvals')} subject={subject} attention={props.waiting.length}>
        <TabBody>
          <ApprovalsPane waiting={props.waiting} decided={props.decided} />
        </TabBody>
      </InspectorTab>
      {(props.plan !== null || props.planMode) && (
        <InspectorTab id="plan" title={t('workspace.panel.plan', 'Plan')} subject={subject}>
          <TabBody>
            <Suspense fallback={null}>
              <plan.TodoPane plan={props.plan} />
            </Suspense>
          </TabBody>
        </InspectorTab>
      )}
      {hasProcesses && (
        <InspectorTab id="processes" title={t('workspace.panel.processes', 'Processes')} subject={subject}>
          <TabBody>
            <Suspense fallback={null}>
              <plan.TerminalPane workspaceId={props.workspaceId} />
            </Suspense>
          </TabBody>
        </InspectorTab>
      )}
      {props.hasIndex && (
        <InspectorTab id="index" title={t('workspace.panel.index', 'Index')} subject={subject}>
          <TabBody>
            <IndexPanelHost workspaceId={props.workspaceId} />
          </TabBody>
        </InspectorTab>
      )}
    </>
  );
}
