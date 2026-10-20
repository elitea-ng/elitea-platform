/**
 * The composer footer's controls — where a native chat/IDE app keeps them:
 * the agent (and its version) as one picker, and where the thread runs
 * (this computer, or the cloud) with the actions that move it.
 */
import { useState, type Ref } from 'react';

import CheckOutlinedIcon from '@mui/icons-material/CheckOutlined';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import CloudOutlinedIcon from '@mui/icons-material/CloudOutlined';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import LaptopMacOutlinedIcon from '@mui/icons-material/LaptopMacOutlined';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Divider from '@mui/material/Divider';
import ListSubheader from '@mui/material/ListSubheader';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import type { Theme } from '@mui/material/styles';

import type { HandoffExecutor } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';

import type { AgentSelection } from './useAgentSelection';
import type { ThreadHandoff } from './useThreadHandoff';

const chipSx = (theme: Theme) => ({
  minHeight: 22,
  gap: 0.5,
  padding: '0 6px',
  borderRadius: '6px',
  fontSize: '0.75rem',
  fontWeight: 500,
  color: theme.vars.palette.text.secondary,
  maxWidth: 240,
  '&:hover, &[aria-expanded="true"]': { background: `color-mix(in srgb, ${theme.vars.palette.text.primary} 8%, transparent)` },
  '& svg': { fontSize: 14 },
});

const ellipsis = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } as const;

export function AgentChip({ selection, agentRef }: { selection: AgentSelection; agentRef?: Ref<HTMLDivElement> | undefined }): React.JSX.Element | null {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const [versionAnchor, setVersionAnchor] = useState<HTMLElement | null>(null);
  if (selection.empty) return null;
  const agent = selection.agents.find((a) => a.id === selection.agentId);
  const version = selection.versions.find((v) => String(v.id) === selection.versionId);
  const close = (): void => {
    setVersionAnchor(null);
    setAnchor(null);
  };
  const label = agent === undefined ? t('workspace.agent', 'Agent') : version === undefined ? agent.name : `${agent.name} · ${version.name}`;
  return (
    <Box ref={agentRef} sx={{ display: 'inline-flex', minWidth: 0 }}>
      <ButtonBase
        data-testid="agent-chip"
        aria-haspopup="menu"
        aria-expanded={anchor !== null}
        aria-label={t('workspace.agent.choose', 'Agent: {{label}}', { label })}
        onClick={(event) => setAnchor(event.currentTarget)}
        sx={chipSx}
      >
        <Box component="span" sx={ellipsis}>
          {label}
        </Box>
        <ExpandMoreIcon />
      </ButtonBase>
      <Menu anchorEl={anchor} open={anchor !== null} onClose={close} anchorOrigin={{ vertical: 'top', horizontal: 'left' }} transformOrigin={{ vertical: 'bottom', horizontal: 'left' }} slotProps={{ list: { dense: true } }}>
        <ListSubheader sx={{ lineHeight: '22px', background: 'transparent' }}>{t('workspace.agent', 'Agent')}</ListSubheader>
        {selection.agents.map((a) => (
          <MenuItem
            key={a.id}
            selected={a.id === selection.agentId}
            onClick={() => {
              selection.selectAgent(a.id);
              close();
            }}
          >
            <Box component="span" sx={{ width: 18, display: 'inline-flex' }}>
              {a.id === selection.agentId && <CheckOutlinedIcon fontSize="inherit" />}
            </Box>
            {a.name}
          </MenuItem>
        ))}
        <Divider />
        <MenuItem disabled={selection.versions.length === 0} aria-haspopup="menu" onClick={(event) => setVersionAnchor(event.currentTarget)} sx={{ gap: 1 }}>
          <Box component="span" sx={{ flex: 1 }}>
            {t('workspace.version', 'Version')}
          </Box>
          <Box component="span" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
            {version?.name ?? ''}
          </Box>
          <ChevronRightIcon fontSize="inherit" />
        </MenuItem>
      </Menu>
      <Menu
        anchorEl={versionAnchor}
        open={versionAnchor !== null}
        onClose={close}
        anchorOrigin={{ vertical: 'top', horizontal: 'right' }}
        transformOrigin={{ vertical: 'top', horizontal: 'left' }}
        slotProps={{ list: { dense: true }, root: { slotProps: { backdrop: { invisible: true } } } }}
      >
        {selection.versions.map((v) => (
          <MenuItem
            key={v.id}
            selected={String(v.id) === selection.versionId}
            onClick={() => {
              selection.selectVersion(String(v.id));
              close();
            }}
          >
            <Box component="span" sx={{ width: 18, display: 'inline-flex' }}>
              {String(v.id) === selection.versionId && <CheckOutlinedIcon fontSize="inherit" />}
            </Box>
            {v.name}
          </MenuItem>
        ))}
      </Menu>
    </Box>
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

/** "⌂ Local ⌄": where the thread runs; the menu moves it. */
export function RunLocationChip({ handoff }: { handoff: ThreadHandoff }): React.JSX.Element | null {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  if (handoff.ipc === undefined || (handoff.status === undefined && !handoff.canSend)) return null;
  const executor: HandoffExecutor = handoff.status?.executor ?? 'local';
  const label = locationLabel(executor);
  const close = (): void => setAnchor(null);
  const interactive = handoff.canSend || handoff.offLaptop;
  const run = (action: () => void) => (): void => {
    close();
    action();
  };
  return (
    <>
      <ButtonBase
        data-testid="executor-badge"
        data-executor={executor}
        disabled={!interactive}
        aria-haspopup="menu"
        aria-expanded={anchor !== null}
        aria-label={t('workspace.handoff.where', 'Runs: {{where}}', { where: label })}
        onClick={(event) => setAnchor(event.currentTarget)}
        sx={(theme: Theme) => ({ ...chipSx(theme), ...(executor === 'local' ? {} : { color: theme.vars.palette.primary.main }) })}
      >
        {executor === 'local' ? <LaptopMacOutlinedIcon /> : <CloudOutlinedIcon />}
        <Box component="span">{label}</Box>
        <ExpandMoreIcon />
      </ButtonBase>
      <Menu anchorEl={anchor} open={anchor !== null} onClose={close} anchorOrigin={{ vertical: 'top', horizontal: 'left' }} transformOrigin={{ vertical: 'bottom', horizontal: 'left' }} slotProps={{ list: { dense: true } }}>
        {handoff.canSend && <MenuItem onClick={run(() => handoff.open('send'))}>{t('workspace.handoff.sendMenu', 'Send to cloud…')}</MenuItem>}
        {handoff.offLaptop && <MenuItem onClick={run(() => handoff.open('takeBack'))}>{t('workspace.handoff.takeBackMenu', 'Take back…')}</MenuItem>}
        {handoff.offLaptop && <MenuItem onClick={run(() => handoff.open('erase'))}>{t('workspace.handoff.eraseMenu', 'Erase cloud copy…')}</MenuItem>}
        {handoff.canSend && <Divider />}
        {handoff.canSend && <MenuItem onClick={run(() => handoff.open('settings'))}>{t('workspace.handoff.settingsMenu', 'Handoff settings…')}</MenuItem>}
      </Menu>
    </>
  );
}
