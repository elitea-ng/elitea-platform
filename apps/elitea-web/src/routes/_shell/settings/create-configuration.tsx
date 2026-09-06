/**
 * ROUTE-063 `/settings/create-configuration` -> `CreateCredentialFromMain`
 * (title "New Configuration", `showCategory=false`, `forceShowTitle` —
 * spec §8.1), wrapped in `IntegrationGuard` (PERM-059). Pattern-A parent of
 * `create-configuration/:credentialType` (ROUTE-064): `beforeLoad` cascades
 * to the child the same way `skillsGuardBeforeLoad` does for the skills
 * family.
 */
import { createFileRoute, Outlet, useNavigate, useParams } from '@tanstack/react-router';
import { useCallback } from 'react';

import { CreateCredential } from '@/pages/credentials/CreateCredential';
import { integrationGuardBeforeLoad } from '@/routes/-guards/integrationGuard';
import { useCredentialFormContext } from '@/routes/-lib/useCredentialFormContext';
import { pickParams } from '@/routes/-search/params';
import { RouteError, RoutePending } from '@/routes/-ui/RouteStatus';

/**
 * Same page as `/credentials/create-credential` (ROUTE-023), with
 * `configurationMode` ON — that flag is what turns "New Credential" into
 * "New Configuration" and hides the category selector
 * (`CredentialFormMode.configurationMode`). Leaving returns to the
 * AI-configuration settings screen these are managed from, not to
 * `/credentials`.
 *
 * `:credentialType` (ROUTE-064) is threaded for the same reason its
 * `/credentials` twin threads it — see `create-credential.tsx`'s header for
 * the baseline citations. The settings side has its own deep-link producer:
 * `hooks/credentials/useCredentialSearch.js:29` picks
 * `CreateConfigurationWithType` over `CreateCredentialTypeFromMain`
 * whenever the user is inside the settings route.
 */
function CreateConfigurationRoute() {
  const navigate = useNavigate();
  const context = useCredentialFormContext();
  const { credentialType } = useParams({ strict: false });
  /**
   * DEFECT: this route declared none of PARAM-037/039/041/043/045 and read
   * none of them, so a deep link that names a section or a prefilled
   * credential lost it. See `credentials/create-credential.tsx` for the twin.
   */
  const { prefill_id: prefillId, prefill_name: prefillName, section } = Route.useSearch();
  // `savedId` is only known on a successful save (`onCreated`) — `onCancelled`
  // calls this with no argument, so Cancel still lands on a plain
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
  // Navigates to ROUTE-064, not ROUTE-024 — `useCredentialSearch.js:29`
  // switches destination on `isFromSettings` for exactly this reason: a type
  // picked inside settings must stay inside settings, or the user lands on
  // the credentials-domain screen with `configurationMode` silently off.
  const chooseType = useCallback(
    (type: string) => {
      void navigate({ to: '/settings/create-configuration/$credentialType', params: { credentialType: type } });
    },
    [navigate],
  );

  return (
    <>
      <CreateCredential
        context={context}
        {...(credentialType !== undefined ? { credentialType } : {})}
        {...(prefillId !== '' ? { prefillId } : {})}
        {...(prefillName !== '' ? { prefillName } : {})}
        {...(section !== '' ? { section } : {})}
        configurationMode
        onCreated={leave}
        onCancelled={leave}
        onTypeChosen={chooseType}
      />
      <Outlet />
    </>
  );
}

export const Route = createFileRoute('/_shell/settings/create-configuration')({
  validateSearch: pickParams('forceCustom', 'from', 'prefill_id', 'prefill_name', 'section'),
  beforeLoad: integrationGuardBeforeLoad,
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: CreateConfigurationRoute,
});
