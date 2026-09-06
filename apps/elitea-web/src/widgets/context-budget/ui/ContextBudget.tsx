/**
 * ui/ContextBudget.tsx — the container: resolves the context status for one
 * conversation and hands the narrowed stats to `ContextBudgetPanel`.
 *
 * Mounted through `ParticipantsWrapper`'s `renderContextBudget` slot (the
 * `features/` layer may not import `widgets/`, so the page supplies the slot —
 * see `pages/chat/index.tsx`). Old-app equivalent:
 * `widgets/context-budget/ui/ContextBudgetInfo.jsx`.
 *
 * Gating, in the same order as the old app:
 *  - no `conversationId` (a brand-new or playback conversation — the wrapper
 *    already nulls the id for those) or no `projectId` -> the query never runs
 *    and nothing renders;
 *  - loading / error / non-object payload -> nothing renders. The old app
 *    returns `null` for all three rather than showing a skeleton or an error;
 *    a rail that briefly shows an empty budget box is worse than one that
 *    appears when it has something to say.
 *
 * SCOPE (deliberate): read-only. The old app's header also carries a pencil
 * that opens `ContextStrategyModal` — a form over `updateContextStrategy`
 * plus the conversation's instructions, persona and summary-LLM settings.
 * That modal is not built here, and the pencil is therefore ABSENT rather than
 * present-and-inert. Note for whoever builds it: the Go route REPLACES the
 * whole `meta.context_strategy` object with the request body (`jsonb_set` in
 * `conversations.go`'s `UpdateContextStrategy`), so a partial form must merge
 * onto the existing strategy or it will silently drop `summary_llm_settings`.
 */
import type { ReactNode } from 'react';

import { contextManagementApi } from '@/entities/conversation';

import { toContextBudgetStats } from '../lib/contextStatus';
import { ContextBudgetCollapsed, ContextBudgetPanel } from './ContextBudgetPanel';

/** @public */
export interface ContextBudgetProps {
  /** The conversation to report on. Absent for a new/playback conversation — the widget then renders nothing. */
  readonly conversationId?: string | number | undefined;
  /** The project the conversation belongs to. Supplied by the page, which already resolves it. */
  readonly projectId?: string | number | undefined;
  /**
   * Render the minimal collapsed form (a percentage over a 2.25rem status
   * line) instead of the full card — the rail is 3.25rem wide there and the
   * card does not fit. Baseline: `ContextBudgetInfo.jsx`'s `if (collapsed)
   * return <ContextBudgetCollapsed/>` branch, driven by
   * `Participants.jsx:120`'s `collapsed && !isSmallWindow`.
   */
  readonly collapsed?: boolean;
}

export function ContextBudget({ conversationId, projectId, collapsed = false }: ContextBudgetProps): ReactNode {
  const enabled = conversationId !== undefined && conversationId !== '' && projectId !== undefined && projectId !== '';
  const { data, isPending, isError } = contextManagementApi.useGetStatus(
    { projectId: projectId ?? '', conversationId: conversationId ?? '' },
    { enabled },
  );

  if (!enabled || isPending || isError) return null;

  const stats = toContextBudgetStats(data);
  if (!stats) return null;

  if (collapsed) return <ContextBudgetCollapsed stats={stats} />;
  return <ContextBudgetPanel stats={stats} />;
}
