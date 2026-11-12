/**
 * The read-only sub-agents one `spawn_agent` call started (#1226), under its
 * tool row: one lane each, with its status, its own tool rows and text, and
 * its report. A lane is open while its child runs and folds once it settled.
 * The lane itself is the shared `SubAgentLaneCard` (issue 1924), the one the
 * cloud chat answer draws too; this file renders its items with the
 * transcript's own tool row.
 */
import type { ComponentType } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { SubAgentLaneCard } from '@/shared/ui/sub-agent-lane';

import { briefRefsLine, type SubAgentLane } from '../model/subAgentEvents';
import type { TranscriptItem } from '../model/transcriptItem';

type ToolItem = Extract<TranscriptItem, { type: 'tool' }>;

/** The avatar a lane's host draws for it (the cloud transcript's: a temporary agent's dashed ring). */
export type SubAgentAvatar = ComponentType<{ lane: SubAgentLane }>;

function Lane({ lane, ToolRow, Avatar }: { lane: SubAgentLane; ToolRow: ComponentType<{ item: ToolItem }>; Avatar?: SubAgentAvatar | undefined }): React.JSX.Element {
  const briefLine = briefRefsLine(lane.briefRefs);
  return (
    <SubAgentLaneCard lane={lane} variant="transcript" avatar={Avatar === undefined ? undefined : <Avatar lane={lane} />}>
      {briefLine === undefined ? null : (
        <Typography variant="bodySmall" component="p" data-testid="subagent-brief-refs" sx={(theme: Theme) => ({ margin: 0, color: theme.vars.palette.text.secondary })}>
          {briefLine}
        </Typography>
      )}
      {lane.items.map((item) =>
        item.type === 'tool' ? (
          <ToolRow key={item.key} item={item} />
        ) : item.type === 'text' ? (
          <Typography key={item.key} variant="bodySmall" component="p" sx={(theme: Theme) => ({ margin: 0, whiteSpace: 'pre-wrap', color: theme.vars.palette.text.secondary })}>
            {item.text}
          </Typography>
        ) : null,
      )}
    </SubAgentLaneCard>
  );
}

export interface SubAgentLanesProps {
  lanes: readonly SubAgentLane[];
  /** The transcript's own tool row, for the children's calls. */
  ToolRow: ComponentType<{ item: ToolItem }>;
  /** Each lane's avatar; absent: none. */
  Avatar?: SubAgentAvatar | undefined;
}

export function SubAgentLanes({ lanes, ToolRow, Avatar }: SubAgentLanesProps): React.JSX.Element | null {
  if (lanes.length === 0) return null;
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }} data-testid="subagent-lanes">
      {lanes.map((lane) => (
        <Lane key={lane.agentId} lane={lane} ToolRow={ToolRow} Avatar={Avatar} />
      ))}
    </Box>
  );
}
