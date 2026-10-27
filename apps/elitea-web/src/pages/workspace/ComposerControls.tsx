/**
 * The composer footer's controls — where a native chat/IDE app keeps them,
 * each drawn as a native pop-up button (`ComposerPicker`, rows from
 * `native-menu`): the agent (and its version) as one picker, and where the
 * thread runs (this computer, or the cloud) with the actions that move it.
 */
import { useState, type Ref } from 'react';

import CheckOutlinedIcon from '@mui/icons-material/CheckOutlined';
import ChevronLeftIcon from '@mui/icons-material/ChevronLeft';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import CloudOutlinedIcon from '@mui/icons-material/CloudOutlined';
import LaptopMacOutlinedIcon from '@mui/icons-material/LaptopMacOutlined';
import Box from '@mui/material/Box';
import Tooltip from '@mui/material/Tooltip';

import type { HandoffExecutor } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';
import { ComposerPicker } from '@/shared/ui/native-composer';
import { MenuDivider, MenuHeader, MenuRow, MenuRowHint, MenuRowIcon, MenuRowText } from '@/shared/ui/native-menu';

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

/** The agent menu's rows: the agents, then "Version ›", which turns the menu into the versions (and back). */
function AgentMenu({ selection, close }: { selection: AgentSelection; close: () => void }): React.JSX.Element[] {
  const [versions, setVersions] = useState(false);
  const version = selection.versions.find((v) => String(v.id) === selection.versionId);
  const pick = (action: () => void) => (): void => {
    close();
    action();
  };
  if (versions) {
    return [
      <MenuRow key="back" onClick={() => setVersions(false)}>
        <MenuRowIcon>
          <ChevronLeftIcon />
        </MenuRowIcon>
        <MenuRowText>{t('workspace.version', 'Version')}</MenuRowText>
      </MenuRow>,
      ...selection.versions.map((v) => <ChoiceRow key={v.id} chosen={String(v.id) === selection.versionId} label={v.name} onClick={pick(() => selection.selectVersion(String(v.id)))} />),
    ];
  }
  return [
    <MenuHeader key="header">{t('workspace.agent', 'Agent')}</MenuHeader>,
    ...selection.agents.map((a) => <ChoiceRow key={a.id} chosen={a.id === selection.agentId} label={a.name} onClick={pick(() => selection.selectAgent(a.id))} />),
    <MenuDivider key="divider" />,
    <MenuRow key="version" disabled={selection.versions.length === 0} aria-haspopup="menu" onClick={() => setVersions(true)}>
      <MenuRowIcon />
      <MenuRowText>{t('workspace.version', 'Version')}</MenuRowText>
      <MenuRowHint>{version?.name ?? ''}</MenuRowHint>
      <MenuRowIcon>
        <ChevronRightIcon />
      </MenuRowIcon>
    </MenuRow>,
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

function locationLabel(executor: HandoffExecutor): string {
  switch (executor) {
    case 'local':
      return t('workspace.handoff.badge.local', 'Local');
    case 'cloud':
      return t('workspace.handoff.badge.cloud', 'In cloud');
    case 'returning':
      return t('workspace.handoff.badge.returning', 'Returning');
  }
}

/** The run-location menu's rows: the actions that move the thread, by where it is. */
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
  if (handoff.canSend) rows.push(row('send', t('workspace.handoff.sendMenu', 'Send to cloud…'), () => handoff.open('send')));
  if (handoff.offLaptop) {
    rows.push(row('takeBack', t('workspace.handoff.takeBackMenu', 'Take back…'), () => handoff.open('takeBack')));
    rows.push(row('erase', t('workspace.handoff.eraseMenu', 'Erase cloud copy…'), () => handoff.open('erase')));
    if (onOpenInChat !== undefined) rows.push(row('chat', t('workspace.openInChat', 'Open in chat'), onOpenInChat));
  }
  if (handoff.canSend) rows.push(<MenuDivider key="divider" />, row('settings', t('workspace.handoff.settingsMenu', 'Handoff settings…'), () => handoff.open('settings')));
  return rows;
}

/** "💻 Local ⌄": where the thread runs; the menu moves it. */
export function RunLocationChip({ handoff, onOpenInChat }: { handoff: ThreadHandoff; onOpenInChat?: (() => void) | undefined }): React.JSX.Element | null {
  if (handoff.ipc === undefined || (handoff.status === undefined && !handoff.canSend)) return null;
  const executor: HandoffExecutor = handoff.status?.executor ?? 'local';
  return (
    <ComposerPicker
      label={t('workspace.handoff.whereLabel', 'Runs')}
      value={locationLabel(executor)}
      icon={executor === 'local' ? <LaptopMacOutlinedIcon /> : <CloudOutlinedIcon />}
      testId="executor-badge"
      disabled={!(handoff.canSend || handoff.offLaptop)}
    >
      {(close) => locationRows(handoff, close, onOpenInChat)}
    </ComposerPicker>
  );
}
