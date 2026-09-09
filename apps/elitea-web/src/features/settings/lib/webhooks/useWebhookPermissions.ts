/**
 * Resolves what the caller may do on the project Webhooks surface (#876).
 *
 * Same shape as `features/settings/lib/secrets/useSecretPermissions.ts`:
 * `canList` gates the LISTING QUERY (a caller with no list permission must
 * not be asked at all), and `controls` is the four write/read gates the page
 * hands to the table so a control that can only 403 is not rendered.
 *
 * The five strings this reads are `PERMISSIONS.webhooks.*` —
 * `configurations.configuration*`, reused rather than invented; see that
 * constant's own doc comment and `internal/api/webhook/handler.go`'s file
 * header for why.
 */
import { useMemo } from 'react';

import type { Permission } from '@/shared/api/generated/model';
import { usePermissionList } from '@/shared/api/generated/auth/auth';
import { PERMISSIONS } from '@/shared/lib/permissions';

export interface WebhookPermissions {
  /** `configurations.configuration.details` — read one webhook, including its live secret. */
  readonly canReveal: boolean;
  /** `configurations.configuration.create` */
  readonly canCreate: boolean;
  /** `configurations.configuration.update` — rotate the secret, toggle active, edit url/events. */
  readonly canUpdate: boolean;
  /** `configurations.configuration.delete` */
  readonly canDelete: boolean;
}

export interface WebhooksSurfacePermissions {
  /** `configurations.configurations.list` — gates the query. */
  readonly canList: boolean;
  readonly controls: WebhookPermissions;
}

export function useWebhookPermissions(projectId: string): WebhooksSurfacePermissions {
  const permissionQuery = usePermissionList(projectId, { query: { enabled: !!projectId } });

  return useMemo(() => {
    const list = permissionQuery.data?.data as Permission[] | undefined;
    const granted = new Set((list ?? []).filter((entry) => entry.enabled).map((entry) => entry.name));
    return {
      canList: granted.has(PERMISSIONS.webhooks.list),
      controls: {
        canReveal: granted.has(PERMISSIONS.webhooks.details),
        canCreate: granted.has(PERMISSIONS.webhooks.create),
        canUpdate: granted.has(PERMISSIONS.webhooks.update),
        canDelete: granted.has(PERMISSIONS.webhooks.delete),
      },
    };
  }, [permissionQuery.data]);
}
