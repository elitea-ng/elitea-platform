import type { ReactNode } from 'react';

import { RailAuthorCard, shouldShowAuthorCard, useRailViewer } from '@/shared/ui/EntityRail';

/**
 * The author card under the toolkits / MCPs list's Filters
 * (`./ToolkitsFilters.tsx`) — the real author card (`shared/ui/EntityRail`),
 * the baseline's `AuthorInformation` in `RightInfoPanel.jsx:44-48`.
 *
 * A page-owned component because this decision
 * (personal-project predicate + which pathname keys the statistic) is a
 * self-contained unit worth naming. `/toolkits` renders `Toolkits:
 * total_toolkits`; `/mcps` matches no entry in the baseline's own
 * `ROUTE_STATISTIC_MAP`, so the card shows the author with no statistic
 * row — the same nothing the baseline renders there.
 *
 * Renders `null` when the viewer is not looking at their own personal
 * project. "Trending Authors" is deliberately NOT the fallback here: the
 * toolkits/MCP routes carry no `author_id`, so there is no author scope for
 * the baseline's other branch to be about.
 */
export interface ToolkitsAuthorCardProps {
  readonly projectId: string | undefined;
  readonly isMCP: boolean;
}

export function ToolkitsAuthorCard({ projectId, isMCP }: ToolkitsAuthorCardProps): ReactNode {
  const viewer = useRailViewer();
  if (!shouldShowAuthorCard(undefined, projectId, viewer.personalProjectId)) return null;
  return (
    <RailAuthorCard
      authorId={viewer.authorId}
      pathname={isMCP ? '/mcps' : '/toolkits'}
    />
  );
}
