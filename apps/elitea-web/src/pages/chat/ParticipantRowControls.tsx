/**
 * One agent row's controls in a Local work thread's Participants tab (ADR-0061 decisions 2a and 4c, #3055 #3067),
 * in the chat panel's `renderAgentLead` slot (the panel is not forked):
 *
 * - the agent's live state as a small badge with its label ("Running cargo"): its avatar animates only when it is
 *   the thread's active agent, and never under reduced motion;
 * - where it runs, **Local** (here, with the folder) or **Cloud** (the conversation text only), and the Lead chip;
 * - a menu: Make lead / Clear lead, Address (`@Name` into the composer), Run here / Run in the cloud, Edit agent and
 *   Remove from thread. A viewer (`readOnly`, the web and mobile, #3069) gets the badges without the menu.
 */
import type { MouseEvent, ReactNode } from 'react';
import { useState } from 'react';

import MoreVertIcon from '@mui/icons-material/MoreVert';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Divider from '@mui/material/Divider';
import IconButton from '@mui/material/IconButton';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import Typography from '@mui/material/Typography';

import type { AvatarMotion } from '@elitea/client-core/conversation';
import { AgentAvatar } from '@/features/chat-messages';
import { t } from '@/shared/i18n';

/** What one row shows. */
export interface ParticipantRowView {
  readonly participantId: number;
  readonly name: string;
  readonly cloud: boolean;
  readonly isLead: boolean;
  /** The state's label (`activityLabel`); `undefined` before any state arrived. */
  readonly label: string | undefined;
  /** The badge avatar's motion: the active agent's state, else `still`. */
  readonly motion: AvatarMotion;
}

/** What the row's menu does; `undefined` for a viewer. */
export interface ParticipantRowActions {
  /** Two or more agents: only then is there a lead to make. */
  readonly canLead: boolean;
  /** The thread is handed off (ADR-0034): where an agent runs cannot change. */
  readonly executorLocked: boolean;
  readonly disabled: boolean;
  readonly onSetLead: (participantId: number) => void;
  readonly onAddress: (name: string) => void;
  readonly onSetCloud: (participantId: number, cloud: boolean) => void;
  readonly onEdit: (participantId: number) => void;
  readonly onRemove: (participantId: number) => void;
}

function RowMenu({ row, actions }: { readonly row: ParticipantRowView; readonly actions: ParticipantRowActions }): ReactNode {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const stop = (event: MouseEvent) => { event.stopPropagation(); };
  const choose = (action: () => void) => (event: MouseEvent) => {
    event.stopPropagation();
    setAnchor(null);
    action();
  };
  const id = row.participantId;
  return (
    <>
      <IconButton
        size="small"
        disabled={actions.disabled}
        aria-label={t('chat.participants.menu', 'Agent options')}
        data-testid="participant-lead-menu"
        onClick={(event) => { event.stopPropagation(); setAnchor(event.currentTarget); }}
      >
        <MoreVertIcon fontSize="small" />
      </IconButton>
      <Menu anchorEl={anchor} open={anchor !== null} onClose={() => setAnchor(null)} onClick={stop}>
        {actions.canLead && (row.isLead ? (
          <MenuItem onClick={choose(() => actions.onSetLead(0))} data-testid="participant-lead-clear">{t('chat.lead.clear', 'Clear lead')}</MenuItem>
        ) : (
          <MenuItem onClick={choose(() => actions.onSetLead(id))} data-testid="participant-lead-make">{t('chat.lead.make', 'Make lead')}</MenuItem>
        ))}
        <MenuItem onClick={choose(() => actions.onAddress(row.name))} data-testid="participant-address">{t('chat.participants.address', 'Address')}</MenuItem>
        <MenuItem
          disabled={actions.executorLocked}
          onClick={choose(() => actions.onSetCloud(id, !row.cloud))}
          data-testid={row.cloud ? 'participant-run-here' : 'participant-run-cloud'}
        >
          {row.cloud ? t('chat.participants.runHere', 'Run here') : t('chat.participants.runCloud', 'Run in the cloud')}
        </MenuItem>
        <Divider />
        <MenuItem onClick={choose(() => actions.onEdit(id))} data-testid="participant-edit">{t('chat.participants.edit', 'Edit agent')}</MenuItem>
        <MenuItem onClick={choose(() => actions.onRemove(id))} data-testid="participant-remove">{t('chat.participants.remove', 'Remove from thread')}</MenuItem>
      </Menu>
    </>
  );
}

function StateBadge({ row }: { readonly row: ParticipantRowView }): ReactNode {
  if (row.label === undefined) return null;
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5, minWidth: 0 }} data-testid="participant-state" data-participant-id={row.participantId} data-motion={row.motion}>
      <AgentAvatar motion={row.motion} size="0.875rem" />
      {/* 5rem: beside the chips and the menu the name keeps room in the chat panel (issue 3067's visual baseline). */}
      <Typography variant="labelSmall" color="text.secondary" noWrap sx={{ fontKerning: 'none', maxWidth: '5rem' }}>{row.label}</Typography>
    </Box>
  );
}

export interface ParticipantRowControlsProps {
  readonly row: ParticipantRowView;
  readonly actions?: ParticipantRowActions | undefined;
  /** Only the state badge (an ordinary chat, whose lead menu sits beside it). */
  readonly presenceOnly?: boolean;
}

export function ParticipantRowControls({ row, actions, presenceOnly = false }: ParticipantRowControlsProps): ReactNode {
  if (presenceOnly) return <StateBadge row={row} />;
  return (
    <Box
      sx={{ display: 'flex', alignItems: 'center', gap: 0.5, flexShrink: 0, minWidth: 0 }}
      onClick={(event) => { event.stopPropagation(); }}
      data-testid="participant-lead"
      data-participant-id={row.participantId}
    >
      <StateBadge row={row} />
      <Chip
        size="small"
        variant="outlined"
        label={row.cloud ? t('chat.participants.cloud', 'Cloud') : t('chat.participants.local', 'Local')}
        data-testid="participant-executor"
        data-executor={row.cloud ? 'cloud' : 'local'}
      />
      {row.isLead && <Chip size="small" label={t('chat.lead.badge', 'Lead')} data-testid="participant-lead-badge" />}
      {actions !== undefined && <RowMenu row={row} actions={actions} />}
    </Box>
  );
}
