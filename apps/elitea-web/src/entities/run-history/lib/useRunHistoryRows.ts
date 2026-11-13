import { useMemo } from 'react';

import { useListConversations } from '@/shared/api/generated/chat/chat';
import type { ConversationSummary } from '@/shared/api/generated/model';

import type { RunHistoryEntityName } from '../ui/RunHistoryPanel.types';

interface Listing {
  readonly rows: readonly ConversationSummary[];
}

/** Both lists, newest first by start time (`created_at` is one fixed-width UTC string, so it sorts as text), each run once. */
function mergeRuns(visible: readonly ConversationSummary[] | undefined, tests: readonly ConversationSummary[] | undefined): readonly ConversationSummary[] {
  const byId = new Map<number, ConversationSummary>();
  for (const row of [...(visible ?? []), ...(tests ?? [])]) byId.set(row.id, row);
  return [...byId.values()].sort((a, b) => b.created_at.localeCompare(a.created_at));
}

/**
 * The runs of one entity: its ordinary conversations, plus (for an agent) the
 * agent page's own test chats (#1981). Those are filed hidden (`editor_test`),
 * which the ordinary list leaves out by default; the old UI's test chats were
 * ordinary conversations in that very list, so they belong to an agent's
 * history. Read with `hidden=only` and merged. Toolkits have no test chat.
 */
export function useRunHistoryRows(
  projectId: string | undefined,
  entityName: RunHistoryEntityName,
  entityId: string | number | undefined,
): { readonly rows: readonly ConversationSummary[]; readonly isPending: boolean } {
  const enabled = projectId !== undefined && entityId !== undefined;
  const filter = { entity_name: entityName, entity_meta_id: String(entityId ?? ''), limit: 50 };
  const listQuery = useListConversations(projectId ?? '', filter, { query: { enabled } });
  const testQuery = useListConversations(
    projectId ?? '',
    { ...filter, hidden: 'only', source: 'editor_test' },
    { query: { enabled: enabled && entityName === 'application' } },
  );
  // `.data.data`'s declared type includes the error-envelope variant, never reachable here
  // since `eliteaFetch` throws instead of resolving with it (mutator.ts's §3.6 unwrap contract).
  const visible = (listQuery.data?.data as Listing | undefined)?.rows;
  const tests = (testQuery.data?.data as Listing | undefined)?.rows;
  const rows = useMemo(() => mergeRuns(visible, tests), [visible, tests]);
  return { rows, isPending: !enabled || listQuery.isPending };
}
