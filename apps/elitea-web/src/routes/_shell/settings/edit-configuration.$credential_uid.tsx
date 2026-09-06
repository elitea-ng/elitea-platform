/**
 * ROUTE-065 `/settings/edit-configuration/:credential_uid` ->
 * `EditCredentialFromMain` (title "Configuration"). Spec §8.1 note: "the
 * param is `:credential_uid`, while `RouteDefinitions.EditConfiguration`
 * declares `:uid`; the MOUNTED route wins" — this file uses
 * `$credential_uid`, not `$uid`.
 *
 * Same page as `/credentials/:tab/:credential_uid` (ROUTE-025), with
 * `configurationMode` ON. Leaving returns to the AI-configuration settings
 * screen these are managed from, matching its create-side sibling
 * (ROUTE-063).
 */
import { createFileRoute, useNavigate } from '@tanstack/react-router';
import { useCallback } from 'react';

import { EditCredential } from '@/pages/credentials/EditCredential';
import { useCredentialFormContext } from '@/routes/-lib/useCredentialFormContext';
import { RouteError, RoutePending } from '@/routes/-ui/RouteStatus';

function EditConfigurationRoute() {
  const navigate = useNavigate();
  const { credential_uid: credentialUid } = Route.useParams();
  const context = useCredentialFormContext();
  // `savedId` is only known on a successful save (`onSaved`) — `onDiscarded`
  // (delete) calls this with no argument, so it still lands on a plain
  // `/settings/model-configuration` with no `reveal` in the URL.
  const leave = useCallback(
    (savedId?: string) => {
      void navigate({
        to: '/settings/model-configuration',
        ...(savedId !== undefined ? { search: { reveal: savedId } } : {}),
      });
    },
    [navigate],
  );

  return (
    <EditCredential
      context={context}
      credentialUid={credentialUid}
      configurationMode
      onSaved={leave}
      onDiscarded={leave}
    />
  );
}

export const Route = createFileRoute('/_shell/settings/edit-configuration/$credential_uid')({
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: EditConfigurationRoute,
});
