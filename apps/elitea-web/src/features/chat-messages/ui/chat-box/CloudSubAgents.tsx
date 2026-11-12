/**
 * A cloud answer's temporary sub-agents (client contract 1.48, ADR-0050
 * decision 4), under the answer's thinking: one row per child with its
 * dashed-ring avatar (the image of its profile's avatar key, else the brand mark), its profile name, a "Temporary" badge, its steps and
 * tokens and its status; opened, its own tool calls and its final answer.
 * Open while the child runs. The lane is the shared `SubAgentLaneCard`
 * (issue 1924), the one the workspace transcript draws too.
 *
 * Every text here is untrusted model or tool output and renders as plain
 * text (no Markdown, no HTML).
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { CloudSubAgentLane } from '@elitea/client-core/stream';

import { SubAgentLaneCard } from '@/shared/ui/sub-agent-lane';

import { AgentAvatar } from './AgentAvatar';

function toolLine(item: Extract<CloudSubAgentLane['items'][number], { type: 'tool' }>): string {
  const state = item.ok === undefined ? '…' : item.ok ? '✓' : '✗';
  const args = item.argsSummary === undefined ? '' : ` ${item.argsSummary}`;
  const result = item.summary === undefined ? '' : ` → ${item.summary}`;
  return `${state} ${item.tool}${args}${result}`;
}

const mutedSx = (theme: Theme) => ({ margin: 0, whiteSpace: 'pre-wrap' as const, overflowWrap: 'anywhere' as const, color: theme.vars.palette.text.secondary });

function Lane({ lane }: { readonly lane: CloudSubAgentLane }): ReactNode {
  // Every cloud child is a temporary participant (decision 7e).
  return (
    <SubAgentLaneCard lane={{ ...lane, temporary: true }} variant="answer" avatar={<AgentAvatar ephemeral avatarKey={lane.avatar} size="1rem" />}>
      {lane.items.map((item, index) => (
        <Typography key={item.type === 'tool' ? `c${item.callId}` : `t${String(index)}`} variant="bodySmall" component="p" sx={mutedSx}>
          {item.type === 'tool' ? toolLine(item) : item.text}
        </Typography>
      ))}
      {/* #1872: a grandchild (depth 2) nests under its parent's own body, one level in. */}
      {lane.children !== undefined && lane.children.length > 0 && (
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
          {lane.children.map((child) => (
            <Lane key={child.agentId} lane={child} />
          ))}
        </Box>
      )}
    </SubAgentLaneCard>
  );
}

export function CloudSubAgents({ lanes }: { readonly lanes: readonly CloudSubAgentLane[] | undefined }): ReactNode {
  if (lanes === undefined || lanes.length === 0) return null;
  return (
    <Box data-testid="cloud-subagents" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, mb: 0.5 }}>
      {lanes.map((lane) => (
        <Lane key={lane.agentId} lane={lane} />
      ))}
    </Box>
  );
}
