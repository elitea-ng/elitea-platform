/**
 * Agent Hub constants — mirrors `apps/elitea-ui/src/[fsd]/features/agent-hub/lib/constants/agentHub.constants.js`.
 *
 * @public Wave-2 unit A13 surface.
 */
/**
 * The search key the agent share link carries, and the key the catalogue
 * reads back (`routes/_shell/elitea-catalog.tsx`'s `validateSearch`,
 * `AgentHub.tsx`'s deep-link effect).
 *
 * It was deleted in the #71 pull request as unused, which is how it looked:
 * the READ side had shipped and the WRITE side — the copy-link button in
 * `AgentModal` — had not, so the app could open a share link nobody could
 * produce (#80's second adjacent gap). Baseline
 * `apps/elitea-ui/src/[fsd]/features/agent-hub/lib/constants/
 * agentHub.constants.js:23`.
 */
export const AGENT_ID = 'agentId';

export const TRENDING_CATEGORY = 'Trending';
export const MY_LIKED_CATEGORY = 'My Liked';
export const OTHER_CATEGORY = 'Other';
export const PAGE_SIZE = 20;
export const ALL_AGENTS_LIMIT = 1000;

export const LikeUpdateStrategy = {
  USE_SERVER_COUNT: 'USE_SERVER_COUNT' as const,
  OPTIMISTIC_INCREMENT: 'OPTIMISTIC_INCREMENT' as const,
  OPTIMISTIC_DECREMENT: 'OPTIMISTIC_DECREMENT' as const,
} as const;

