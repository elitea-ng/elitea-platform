/**
 * The composer footer's controls — where a native chat/IDE app keeps them,
 * each drawn as a native pop-up button (`ComposerPicker`, rows from
 * `native-menu`): the agent (and its version) as one picker, and where the
 * thread runs (this computer, or the cloud) with the actions that move it.
 */
import type { Ref } from 'react';

import CheckOutlinedIcon from '@mui/icons-material/CheckOutlined';
import AltRouteOutlinedIcon from '@mui/icons-material/AltRouteOutlined';
import CloudUploadOutlinedIcon from '@mui/icons-material/CloudUploadOutlined';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import FolderOutlinedIcon from '@mui/icons-material/FolderOutlined';
import LaptopMacOutlinedIcon from '@mui/icons-material/LaptopMacOutlined';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import type { WorkIn } from '@/features/workspace-worktrees';
import type { HandoffExecutor } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';
import { ComposerMoreMenu, ComposerPicker } from '@/shared/ui/native-composer';
import { MenuDivider, MenuHeader, MenuNote, MenuRow, MenuRowIcon, MenuRowText, NativeSubmenu } from '@/shared/ui/native-menu';

import type { AgentSelection } from './useAgentSelection';
import type { ThreadHandoff } from './useThreadHandoff';

/** A row with a check on the chosen one. */
function ChoiceRow({ chosen, label, onClick }: { chosen: boolean; label: string; onClick: () => void }): React.JSX.Element {
  return (
    <MenuRow selected={chosen} onClick={onClick}>
      <MenuRowIcon>{chosen && <CheckOutlinedIcon />}</MenuRowIcon>
      <MenuRowText>{label}</MenuRowText>
    </MenuRow>
  );
}

/** The agent menu's rows: the agents, then "Version ›", whose versions open beside it on hover. */
function AgentMenu({ selection, close }: { selection: AgentSelection; close: () => void }): React.JSX.Element[] {
  const version = selection.versions.find((v) => String(v.id) === selection.versionId);
  const pick = (action: () => void) => (): void => {
    close();
    action();
  };
  return [
    <MenuHeader key="header">{t('workspace.agent', 'Agent')}</MenuHeader>,
    ...selection.agents.map((a) => <ChoiceRow key={a.id} chosen={a.id === selection.agentId} label={a.name} onClick={pick(() => selection.selectAgent(a.id))} />),
    <MenuDivider key="divider" />,
    <NativeSubmenu key="version" label={t('workspace.version', 'Version')} hint={version?.name ?? ''} disabled={selection.versions.length === 0}>
      {() => selection.versions.map((v) => <ChoiceRow key={v.id} chosen={String(v.id) === selection.versionId} label={v.name} onClick={pick(() => selection.selectVersion(String(v.id)))} />)}
    </NativeSubmenu>,
  ];
}

/** "developer · base ⌄": the agent and its version. `disabledHint`: why it cannot change (a thread in the cloud). */
export function AgentChip({ selection, agentRef, disabledHint }: { selection: AgentSelection; agentRef?: Ref<HTMLDivElement> | undefined; disabledHint?: string | undefined }): React.JSX.Element | null {
  if (selection.empty) return null;
  const agent = selection.agents.find((a) => a.id === selection.agentId);
  const version = selection.versions.find((v) => String(v.id) === selection.versionId);
  const label = agent === undefined ? t('workspace.agent', 'Agent') : version === undefined ? agent.name : `${agent.name} · ${version.name}`;
  return (
    <Tooltip title={disabledHint ?? ''}>
      <Box ref={agentRef} sx={{ display: 'inline-flex', minWidth: 0 }}>
        <ComposerPicker label={t('workspace.agent', 'Agent')} value={label} testId="agent-chip" disabled={disabledHint !== undefined}>
          {(close) => <AgentMenu selection={selection} close={close} />}
        </ComposerPicker>
      </Box>
    </Tooltip>
  );
}

/** The run-location ⌄ menu's rows: what moves the thread besides the main action, by where it is. */
function locationRows(handoff: ThreadHandoff, close: () => void, onOpenInChat: (() => void) | undefined): React.JSX.Element[] {
  const row = (key: string, label: string, action: () => void): React.JSX.Element => (
    <MenuRow
      key={key}
      onClick={() => {
        close();
        action();
      }}
    >
      <MenuRowText>{label}</MenuRowText>
    </MenuRow>
  );
  const rows: React.JSX.Element[] = [];
  if (handoff.offLaptop) {
    rows.push(row('erase', t('workspace.handoff.eraseMenu', 'Erase cloud copy…'), () => handoff.open('erase')));
    if (onOpenInChat !== undefined) rows.push(row('chat', t('workspace.openInChat', 'Open in chat'), onOpenInChat));
  }
  if (handoff.canSend) {
    if (rows.length > 0) rows.push(<MenuDivider key="divider" />);
    rows.push(row('settings', t('workspace.handoff.settingsMenu', 'Handoff settings…'), () => handoff.open('settings')));
  }
  return rows;
}

/** The control's main action by where the thread is: send it, take it back, or (on its way back) nothing yet. */
function locationAction(handoff: ThreadHandoff, executor: HandoffExecutor): { label: string; icon: React.JSX.Element; run?: () => void } | null {
  if (executor === 'returning') return { label: t('workspace.handoff.badge.returning', 'Returning'), icon: <LaptopMacOutlinedIcon /> };
  if (handoff.offLaptop) return { label: t('workspace.handoff.takeBack', 'Take back'), icon: <LaptopMacOutlinedIcon />, run: () => handoff.open('takeBack') };
  if (handoff.canSend) return { label: t('workspace.handoff.send', 'Send to cloud'), icon: <CloudUploadOutlinedIcon />, run: () => handoff.open('send') };
  return null;
}

/**
 * "☁ Send to cloud ⌄" on a thread running here, "💻 Take back ⌄" on one in
 * the cloud: the move as a button, and a small menu beside it for the rest
 * (erase the cloud copy, open it in Chat, the folder's handoff settings).
 * Nothing when the thread cannot move: the composer already says it runs here.
 */
export function RunLocationControl({ handoff, onOpenInChat }: { handoff: ThreadHandoff; onOpenInChat?: (() => void) | undefined }): React.JSX.Element | null {
  if (handoff.ipc === undefined) return null;
  const executor: HandoffExecutor = handoff.status?.executor ?? 'local';
  const action = locationAction(handoff, executor);
  if (action === null) return null;
  const rows = (close: () => void): React.JSX.Element[] => locationRows(handoff, close, onOpenInChat);
  const more = rows(() => undefined).length > 0;
  return (
    <Box data-testid="run-location" data-executor={executor} sx={{ display: 'inline-flex', alignItems: 'center', minWidth: 0 }}>
      <ButtonBase
        data-testid="run-location-action"
        disabled={action.run === undefined}
        onClick={action.run}
        sx={(theme: Theme) => ({
          gap: 0.5,
          height: '1.75rem',
          paddingX: 0.75,
          borderRadius: theme.vars.shape.radiusSm,
          color: theme.vars.palette.text.secondary,
          '& svg': { width: '1rem', height: '1rem', color: theme.vars.palette.text.metrics },
          '&:hover': { background: theme.vars.palette.action.hover },
        })}
      >
        {action.icon}
        <Typography variant="labelSmall" component="span" noWrap>
          {action.label}
        </Typography>
      </ButtonBase>
      {more && (
        <ComposerMoreMenu testId="run-location-more" label={t('workspace.handoff.more', 'Cloud options')} icon={<ExpandMoreIcon />}>
          {rows}
        </ComposerMoreMenu>
      )}
    </Box>
  );
}

const WORK_IN_CHOICES: readonly WorkIn[] = ['folder', 'worktree'];

/**
 * "📁 Folder ⌄" on a new thread of a git folder: work in the folder itself,
 * or in a new task worktree (its own branch from the folder's HEAD, so turns
 * there and in the folder run side by side). The first send makes the worktree.
 */
export function WorkInChip({ value, onChange, disabled }: { value: WorkIn; onChange: (next: WorkIn) => void; disabled?: boolean }): React.JSX.Element {
  const folder = t('workspace.workIn.folder', 'This folder');
  const worktree = t('workspace.workIn.worktree', 'New worktree');
  return (
    <ComposerPicker
      label={t('workspace.workIn.label', 'Work in')}
      value={value === 'worktree' ? worktree : folder}
      icon={value === 'worktree' ? <AltRouteOutlinedIcon /> : <FolderOutlinedIcon />}
      testId="work-in-chip"
      disabled={disabled ?? false}
    >
      {(close) => [
        <MenuHeader key="header">{t('workspace.workIn.label', 'Work in')}</MenuHeader>,
        ...WORK_IN_CHOICES.map((choice) => (
          <MenuRow
            key={choice}
            selected={value === choice}
            data-testid={`work-in-${choice}`}
            onClick={() => {
              close();
              onChange(choice);
            }}
          >
            <MenuRowIcon>{choice === 'worktree' ? <AltRouteOutlinedIcon /> : <FolderOutlinedIcon />}</MenuRowIcon>
            <MenuRowText>{choice === 'worktree' ? worktree : folder}</MenuRowText>
            <MenuRowIcon>{value === choice && <CheckOutlinedIcon />}</MenuRowIcon>
          </MenuRow>
        )),
        <MenuNote key="note">{t('workspace.workIn.note', 'A worktree is a separate copy on its own branch: the agent works there while you keep using the folder. Merge it back from the sidebar.')}</MenuNote>,
      ]}
    </ComposerPicker>
  );
}
