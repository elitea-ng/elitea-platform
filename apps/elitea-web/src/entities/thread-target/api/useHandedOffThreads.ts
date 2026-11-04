/**
 * The threads a schedule or a trigger can target: the caller's Local work
 * threads in this project that are in the cloud now.
 *
 * There is no list of handoffs. `listConversations` with `source=local_work`
 * and `mine=true` lists the caller's Local work threads (Main refuses a thread
 * whose user participant the caller is not), and each row's
 * `workspace_executor` (client contract 1.8) says who runs its turns now:
 * `cloud` is a handed-off thread. A thread that was handed off once and taken
 * back is `local`: Main still accepts it, but it is not offered here, because
 * every occurrence on it would record `skipped_thread_busy`.
 */
import { useMemo } from 'react';

import { useListConversations } from '@/shared/api/generated/chat/chat';
import type { ConversationSummary } from '@/shared/api/generated/model';
import { unwrapList } from '@/shared/api/unwrap';

/** A thread the picker offers. */
interface HandedOffThread {
  readonly uuid: string;
  readonly name: string;
}

/** The most rows one listing returns (v2.yaml `limit.maximum`). */
const LIST_LIMIT = 100;

export interface HandedOffThreads {
  readonly threads: readonly HandedOffThread[];
  /** Every Local work thread of the caller, by uuid: a stored target that is not in the cloud now still has a name. */
  readonly names: ReadonlyMap<string, string>;
  readonly isLoading: boolean;
}

export function useHandedOffThreads(projectId: string | undefined, enabled = true): HandedOffThreads {
  const project = projectId ?? '';
  const query = useListConversations(project, { source: 'local_work', mine: true, limit: LIST_LIMIT, offset: 0 }, { query: { enabled: enabled && project !== '' } });
  const rows = useMemo(() => unwrapList<ConversationSummary>(query.data, 'threadTarget.threads'), [query.data]);
  return useMemo(() => {
    const names = new Map<string, string>();
    const threads: HandedOffThread[] = [];
    for (const row of rows) {
      if (typeof row.uuid !== 'string' || row.uuid === '') continue;
      names.set(row.uuid, row.name);
      if (row.workspace_executor === 'cloud') threads.push({ uuid: row.uuid, name: row.name });
    }
    return { threads, names, isLoading: query.isLoading };
  }, [rows, query.isLoading]);
}
