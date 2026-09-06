import type { ReactNode } from 'react';

import { EntityEmptyState } from '@/shared/ui/EntityCardList';

/**
 * A toolkit-domain-scoped local copy of
 * `apps/elitea-ui/src/[fsd]/entities/empty-state-page/ui/EmptyStatePage.jsx`
 * — the "zero toolkits at all yet, here's a Create CTA" state `ToolkitsList`
 * renders (baseline: `CardList`'s `customEmptyState` prop). NOT one of this
 * sub-unit's owned files (`entities/empty-state-page` has no port anywhere
 * in this app — not promoted, no other domain has built it either), same
 * "no shared home, build a small local copy" precedent
 * `features/agents/ui/AuthorsButton.tsx`'s own doc comment already
 * documents for an identically-situated baseline dependency.
 *
 * DISCLOSED CUT CLOSED: the illustration the baseline renders
 * (`assets/images/Applications_{Dark,Light}_1.png`) now exists in this app,
 * re-encoded as WebP under `src/assets/empty-states/`, and this component is
 * a thin alias over `shared/ui/EntityEmptyState`, which paints it. Kept as a
 * named toolkit-domain component so the slice's own callers (and its tests)
 * keep their import.
 */
export interface ToolkitsEmptyStateProps {
  readonly title: string;
  readonly description: string;
  readonly onCreateClick: () => void;
  readonly onGuidedTourClick?: () => void;
}

export function ToolkitsEmptyState({ title, description, onCreateClick, onGuidedTourClick }: ToolkitsEmptyStateProps): ReactNode {
  return (
    <EntityEmptyState
      art="applications"
      title={title}
      description={description}
      onCreateClick={onCreateClick}
      {...(onGuidedTourClick === undefined ? {} : { onGuidedTourClick })}
    />
  );
}
