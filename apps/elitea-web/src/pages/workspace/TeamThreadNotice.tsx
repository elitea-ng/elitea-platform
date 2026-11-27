/**
 * Above the composer of a Local work thread run by a saved team (#3053): who answers and where each agent runs. Every
 * member runs on this computer with the folder and its tools, and so does an agent a member consults or hands the floor
 * to (ADR-0061 decisions 3d, 3e). Only an agent that cannot run here runs in the cloud as a child turn and gets no folder
 * (ADR-0029 D0): the notice says so, rather than the folder being left out of that agent's turn without a word.
 *
 * One line (#3145): who a message goes to, with "Learn more" for where the members run, and a close button. Closing
 * it hides it for this thread (kept on this computer once the thread exists; a new thread's choice moves to it with
 * the first message, as the composer's other choices do not need to: it is only shown again in another thread).
 */
import { useEffect, useState } from 'react';

import CloseIcon from '@mui/icons-material/Close';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { createStorage } from '@/shared/lib/storage';

import type { WorkTeam } from './workTeams';

export interface TeamThreadNoticeProps {
  team: WorkTeam | undefined;
  /** A local thread (not the cloud copy, not a pipeline): only there do the members run on this computer. */
  local: boolean;
  /** The thread: closing the notice hides it for this one. `''`: a new thread (hidden until it is left). */
  conversationId: string;
}

const dismissedKey = (conversationId: string): string => `el.desktop.thread.${conversationId}.teamNoticeClosed`;

function readDismissed(conversationId: string): boolean {
  if (conversationId === '') return false;
  try {
    return createStorage('local').get(dismissedKey(conversationId)) === '1';
  } catch {
    return false;
  }
}

function writeDismissed(conversationId: string): void {
  if (conversationId === '') return;
  try {
    createStorage('local').set(dismissedKey(conversationId), '1');
  } catch {
    // Not kept: it is hidden for this visit only.
  }
}

/** Closed for this thread: read again when the thread changes (a new thread gets its id with its first message). */
function useDismissed(conversationId: string): [boolean, () => void] {
  const [closed, setClosed] = useState<{ id: string; closed: boolean }>(() => ({ id: conversationId, closed: readDismissed(conversationId) }));
  if (closed.id !== conversationId) {
    // The new thread's first message named it: a notice closed before then stays closed (kept for it below).
    setClosed({ id: conversationId, closed: closed.closed && closed.id === '' ? true : readDismissed(conversationId) });
  }
  useEffect(() => {
    if (closed.closed && closed.id !== '') writeDismissed(closed.id);
  }, [closed]);
  return [closed.closed, () => {
    writeDismissed(conversationId);
    setClosed({ id: conversationId, closed: true });
  }];
}

export function TeamThreadNotice({ team, local, conversationId }: TeamThreadNoticeProps): React.JSX.Element | null {
  const [closed, close] = useDismissed(conversationId);
  const [more, setMore] = useState(false);
  if (team === undefined || !local || closed) return null;
  const lead = team.primary.name;
  const line = team.lead
    ? t('workspace.team.leadNotice', 'Team {{team}}: a message that names no agent goes to {{lead}} (lead); write @Name to address another member.', { team: team.name, lead })
    : t('workspace.team.addressedNotice', 'Team {{team}}: write @Name to address a member; a message that names no agent goes to the agent that answered last ({{lead}} at first).', { team: team.name, lead });
  return (
    <Alert
      severity="info"
      data-testid="workspace-team-notice"
      sx={{ paddingY: 0, alignItems: 'center', '& .MuiAlert-message': { minWidth: 0, flex: 1, paddingY: 0.5 } }}
      action={
        <IconButton size="small" color="inherit" aria-label={t('workspace.team.noticeClose', 'Hide for this thread')} title={t('workspace.team.noticeClose', 'Hide for this thread')} onClick={close} data-testid="workspace-team-notice-close">
          <CloseIcon fontSize="small" />
        </IconButton>
      }
    >
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, minWidth: 0 }}>
        <Typography variant="bodySmall" noWrap={!more} title={more ? undefined : line} sx={{ minWidth: 0, flex: 1 }}>
          {line}
        </Typography>
        <Button size="small" color="inherit" aria-expanded={more} onClick={() => setMore((open) => !open)} data-testid="workspace-team-notice-more" sx={{ flexShrink: 0, textTransform: 'none', textDecoration: 'underline' }}>
          {more ? t('workspace.team.noticeLess', 'Show less') : t('workspace.team.noticeMore', 'Learn more')}
        </Button>
      </Box>
      {more && (
        <Typography variant="bodySmall" component="p" sx={{ marginTop: 0.5 }} data-testid="workspace-team-notice-details">
          {t(
            'workspace.team.folderNoticeLocal',
            'Every member works on this computer with this folder and its tools, also when another member asks it or hands it the floor. Only an agent that cannot run on this computer runs in the cloud without the folder.',
          )}
        </Typography>
      )}
    </Alert>
  );
}
