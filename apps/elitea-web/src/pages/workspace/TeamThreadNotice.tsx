/**
 * Above the composer of a Local work thread run by a saved team (#3053): who answers and where each agent runs. Every
 * member runs on this computer with the folder and its tools. An agent that a member calls as a tool (a nested agent)
 * runs in the cloud as a child turn and gets no folder (ADR-0029 D0): the notice says so, rather than the folder being
 * left out of that agent's turn without a word.
 */
import Alert from '@mui/material/Alert';

import { t } from '@/shared/i18n';

import type { WorkTeam } from './workTeams';

export interface TeamThreadNoticeProps {
  team: WorkTeam | undefined;
  /** A local thread (not the cloud copy, not a pipeline): only there do the members run on this computer. */
  local: boolean;
}

export function TeamThreadNotice({ team, local }: TeamThreadNoticeProps): React.JSX.Element | null {
  if (team === undefined || !local) return null;
  const lead = team.primary.name;
  return (
    <Alert severity="info" data-testid="workspace-team-notice">
      {team.lead
        ? t('workspace.team.leadNotice', 'Team {{team}}: a message that names no agent goes to {{lead}} (lead); write @Name to address another member.', { team: team.name, lead })
        : t('workspace.team.addressedNotice', 'Team {{team}}: write @Name to address a member; a message that names no agent goes to the agent that answered last ({{lead}} at first).', { team: team.name, lead })}{' '}
      {t(
        'workspace.team.folderNotice',
        'Every member works on this computer with this folder and its tools. An agent a member calls as a tool runs in the cloud and does not get the folder.',
      )}
    </Alert>
  );
}
