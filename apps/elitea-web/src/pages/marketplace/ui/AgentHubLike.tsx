/**
 * Agent Hub Like — like/unlike button for agent cards.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/features/agent-hub/ui/AgentHubLike.jsx`.
 *
 * Deviations:
 *  - Uses `useCardLike` from entities/application/model instead of the old
 *    Redux-based `Like` component and AgentHubContext. `useCardLike` now
 *    calls the real `/social/like` endpoint (adversarial-review fix,
 *    cluster A13-agents-hub, finding 1) — see that hook's own doc comment
 *    for the disclosed, out-of-this-cluster's-scope backend defect that
 *    still makes the call 500 today.
 *  - Uses heart icons from shared/ui/icons.
 *  - No tour IDs.
 */
import { memo } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { HeartIcon } from '@/shared/ui/icons/heart-icon';
import { HeartActiveIcon } from '@/shared/ui/icons/heart-active-icon';
import { useCardLike } from '@/entities/application';

import type { ApplicationData } from '../types';

interface AgentHubLikeProps {
  data: ApplicationData;
}

const AgentHubLike = memo(({ data }: AgentHubLikeProps) => {
  const { isLiked, likeCount, isToggling, toggleLike } = useCardLike({
    applicationId: data.id,
    // `project_id` is always the public project on every agents-hub card
    // (see `useCardLike`'s own `projectId` doc comment) — read straight off
    // the card's own data rather than re-deriving it here.
    projectId: data.project_id,
    initialLiked: data.is_liked ?? false,
    initialCount: data.likes ?? 0,
  });

  return (
    <Box
      component="button"
      type="button"
      aria-pressed={isLiked}
      disabled={isToggling}
      // The card is a button that opens the agent; a like must not open it (issue 2028).
      onClick={(event) => { event.stopPropagation(); void toggleLike(); }}
      sx={{ cursor: 'pointer', display: 'flex', alignItems: 'center', gap: 0.5, background: 'none', border: 0, padding: 0, color: 'inherit', font: 'inherit' }}
    >
      {isLiked ? <HeartActiveIcon /> : <HeartIcon />}
      <Typography variant="bodySmall">{likeCount}</Typography>
    </Box>
  );
});

AgentHubLike.displayName = 'AgentHubLike';

export default AgentHubLike;
