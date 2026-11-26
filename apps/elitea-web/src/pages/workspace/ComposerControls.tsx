/**
 * The composer footer's controls — where a native chat/IDE app keeps them,
 * each drawn as a native pop-up button (`ComposerPicker`, rows from
 * `native-menu`): who the message goes to (`AgentChip`, which opens the
 * thread's Participants tab, #3081), and where the thread runs (this
 * computer, or the cloud) with the actions that move it.
 */
import type { Ref } from 'react';

import CheckOutlinedIcon from '@mui/icons-material/CheckOutlined';
import GroupsOutlinedIcon from '@mui/icons-material/GroupsOutlined';
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

import { addressedTo } from '@elitea/client-core/conversation';
import type { WorkIn } from '@/features/workspace-worktrees';
import type { HandoffExecutor } from '@/shared/desktop/handoffIpc';
import { useSidePanelSlot } from '@/shared/desktop/inspectorSlot';
import { t } from '@/shared/i18n';
import { ComposerMoreMenu, ComposerPicker } from '@/shared/ui/native-composer';
import { MenuDivider, MenuHeader, MenuNote, MenuRow, MenuRowIcon, MenuRowText } from '@/shared/ui/native-menu';

import { participantsTabId, pendingAgents } from './ThreadAgentsPanel';
import type { AgentSelection } from './useAgentSelection';
import type { ThreadAgent } from './useEnsureConversation';
import type { ThreadHandoff } from './useThreadHandoff';
import { toTarget } from './WorkspaceToChip';

/** What the chip needs of a thread that exists: its agents and `meta` (the lead), as Main routes a message. */
interface ChipFloor {
  readonly agents: readonly ThreadAgent[];
  readonly meta: unknown;
}

/** "To: Name" or "To: Name (lead)". */
function toLabel(name: string, lead: boolean): string {
  return lead ? t('widgets.chatBox.floorTarget.lead', 'To: {{name}} (lead)', { name }) : t('widgets.chatBox.floorTarget.label', 'To: {{name}}', { name });
}

/** Who the message being typed goes to in a new thread of several agents: the first one it names, else the composer's agent. */
function pendingTarget(selection: AgentSelection, draft: string): string | undefined {
  const pending = pendingAgents(selection, []);
  if (pending.length < 2) return undefined;
  const addressable = pending.map((agent, index) => ({ participantId: index + 1, name: agent.name }));
  const first = addressedTo(draft, addressable)[0];
  const addressed = first === undefined ? undefined : pending[first - 1];
  if (addressed !== undefined) return toLabel(addressed.name, false);
  return toLabel(pending[0]?.name ?? '', selection.team?.lead === true);
}

/**
 * The chip's text (#3081): who the message being typed goes to, by the rule Main routes on (`toTarget`: the first
 * agent it names, else the lead, else the composer's agent), with the team's name; with one agent, the agent and its
 * version ("Coder · base").
 */
export function agentChipLabel(selection: AgentSelection, draft = '', floor?: ChipFloor): string {
  const agent = selection.agents.find((a) => a.id === selection.agentId);
  const sticky = floor?.agents.find((a) => String(a.applicationId) === selection.agentId)?.participantId ?? 0;
  const target = floor === undefined ? undefined : toTarget(draft, floor.agents, floor.meta, sticky);
  const version = selection.versions.find((v) => String(v.id) === selection.versionId);
  const single = agent === undefined ? t('workspace.agent', 'Agent') : version === undefined ? agent.name : `${agent.name} · ${version.name}`;
  const pending = floor === undefined ? pendingTarget(selection, draft) : undefined;
  const label = target === undefined ? (pending ?? single) : toLabel(target.agent.name, target.isLead);
  return selection.team === undefined ? label : `${selection.team.name} · ${label}`;
}

/** How many agents the thread has with the next message: the Participants tab is `team` with two or more. */
function agentCount(selection: AgentSelection, floor: ChipFloor | undefined): number {
  const joined = floor?.agents ?? [];
  return joined.length + pendingAgents(selection, joined).length;
}

interface AgentChipProps {
  readonly selection: AgentSelection;
  /** The message being typed: an "@Name" in it is who it goes to. */
  readonly draft?: string;
  readonly floor?: ChipFloor | undefined;
  readonly agentRef?: Ref<HTMLDivElement> | undefined;
  /** Why it cannot open (a thread in the cloud). */
  readonly disabledHint?: string | undefined;
}

/**
 * "To: Reviewer (lead)": who the next message goes to (#3081). It opens the thread's Participants tab, where its agents
 * are added, removed and made lead; the chip itself holds no menu.
 */
export function AgentChip({ selection, draft = '', floor, agentRef, disabledHint }: AgentChipProps): React.JSX.Element | null {
  const panel = useSidePanelSlot();
  if (selection.empty) return null;
  const label = agentChipLabel(selection, draft, floor);
  return (
    <Tooltip title={disabledHint ?? t('workspace.participants.open', 'Agents of this thread')}>
      <Box ref={agentRef} component="span" sx={{ display: 'inline-flex', minWidth: 0 }}>
        <ButtonBase
          data-testid="agent-chip"
          disabled={disabledHint !== undefined}
          aria-label={`${t('workspace.agent', 'Agent')}: ${label}`}
          onClick={() => panel?.show(participantsTabId(agentCount(selection, floor)))}
          sx={(theme: Theme) => ({
            minWidth: 0,
            gap: 0.5,
            height: '1.75rem',
            paddingX: 0.75,
            borderRadius: theme.vars.shape.radiusSm,
            color: theme.vars.palette.text.secondary,
            '& svg': { width: '1rem', height: '1rem', flexShrink: 0, color: theme.vars.palette.text.metrics },
            '&:hover': { background: theme.vars.palette.action.hover },
          })}
        >
          <GroupsOutlinedIcon aria-hidden />
          <Typography variant="labelSmall" component="span" noWrap sx={{ minWidth: 0 }}>
            {label}
          </Typography>
        </ButtonBase>
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
