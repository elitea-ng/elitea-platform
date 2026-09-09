/**
 * Resolves what the caller may do on the Long-term Memory surface (#870).
 *
 * Same shape as `features/settings/lib/webhooks/useWebhookPermissions.ts`:
 * `canList` gates the LISTING QUERY, and `canWrite` is the single write gate
 * every mutating control (add/edit/delete/clear-all/enable-toggle) checks.
 * One write gate rather than four (webhooks' shape) because
 * `internal/api/v2/memories/handler.go`'s router wiring declares exactly two
 * permission strings total — read and write — not one per verb (see
 * router.go's own comment on why: memory read/write reuses
 * `models.chat.conversation.details`/`.update` rather than inventing four
 * new memories-scoped permissions).
 */
import { useMemo } from 'react';

import type { Permission } from '@/shared/api/generated/model';
import { usePermissionList } from '@/shared/api/generated/auth/auth';
import { PERMISSIONS } from '@/shared/lib/permissions';

export interface LongTermMemoryPermissions {
  /** `models.chat.conversation.details` — gates the listing query. */
  readonly canList: boolean;
  /** `models.chat.conversation.update` — gates every write: add, edit, delete, clear all, enable/disable. */
  readonly canWrite: boolean;
}

export function useLongTermMemoryPermissions(projectId: string): LongTermMemoryPermissions {
  const permissionQuery = usePermissionList(projectId, { query: { enabled: !!projectId } });

  return useMemo(() => {
    const list = permissionQuery.data?.data as Permission[] | undefined;
    const granted = new Set((list ?? []).filter((entry) => entry.enabled).map((entry) => entry.name));
    return {
      canList: granted.has(PERMISSIONS.chat.memories.list),
      canWrite: granted.has(PERMISSIONS.chat.memories.write),
    };
  }, [permissionQuery.data]);
}
