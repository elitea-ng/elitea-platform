/**
 * ROUTE-022 `/credentials/:tab` -> `Credentials`. Query params
 * PARAM-037/039/041/043/045 (`forceCustom`, `from`, `prefill_id`,
 * `prefill_name`, `section`) — inherited by `$tab/:credential_uid`
 * (PARAM-038/040/042/044/046 declare the identical set, no additions).
 *
 * `ExclusiveOutlet` is preserved: `:tab` and `:tab/:credential_uid` are
 * mutually exclusive screens in the baseline, so the list renders only when
 * no detail child is matched.
 *
 * The route owns `projectId` and both navigation callbacks (§3.2). Selecting
 * a row and pressing "new" navigate WITHIN the current tab, so returning
 * from either lands back on the same list — see the two sibling routes'
 * `leave` handlers, which navigate to `/credentials/$tab` for the same
 * reason.
 */
import { createFileRoute, useNavigate } from '@tanstack/react-router';
import { useCallback } from 'react';

import { Credentials } from '@/pages/credentials/Credentials';
import { useSelectedProjectStore } from '@/widgets/app-shell';

import { ExclusiveOutlet } from '../../-ui/ExclusiveOutlet';
import { RouteError, RoutePending } from '../../-ui/RouteStatus';
import { pickParams } from '../../-search/params';

function CredentialsRoute() {
  const navigate = useNavigate();
  const { tab } = Route.useParams();
  const projectId = useSelectedProjectStore((state) => state.project?.id ?? '');

  const onSelectCredential = useCallback((id: string) => {
    void navigate({ to: '/credentials/$tab/$credential_uid', params: { tab, credential_uid: id } });
  }, [navigate, tab]);

  const onCreateNew = useCallback(() => {
    void navigate({ to: '/credentials/create-credential' });
  }, [navigate]);

  return (
    <ExclusiveOutlet>
      <Credentials
        tab={tab}
        projectId={projectId}
        onSelectCredential={onSelectCredential}
        onCreateNew={onCreateNew}
      />
    </ExclusiveOutlet>
  );
}

export const Route = createFileRoute('/_shell/credentials/$tab')({
  validateSearch: pickParams('forceCustom', 'from', 'prefill_id', 'prefill_name', 'section', 'view'),
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: CredentialsRoute,
});
