/**
 * Configuration navigation hook — opens ONE stored configuration for editing.
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/lib/hooks/useConfigurationNavigation.hooks.js`.
 *
 * DEFECT this repairs (gap 6). Every card on Settings -> AI Configuration
 * calls this hook (`ConfigurationSection.tsx`, `ConfigurationCard.tsx`), and
 * it sent the browser to `/settings/create-configuration` — ROUTE-063, the
 * type chooser for a NEW configuration. The id it was given went into a
 * `state.routeStack[].pagePath` breadcrumb string, and nothing in this app
 * reads `routeStack` at all (grep: the only other writer is
 * `AddModelButton.tsx`; there is no reader). So the id was discarded and an
 * existing credential could not be opened, renamed, re-pointed or re-saved
 * from this screen. A user who clicked `local-vllm` got an empty "New
 * Configuration" form.
 *
 * The real editor is ROUTE-065, `/settings/edit-configuration/$credential_uid`
 * (`routes/_shell/settings/edit-configuration.$credential_uid.tsx`). It is the
 * same `EditCredential` page `/credentials/:tab/:credential_uid` (ROUTE-025)
 * mounts, with `configurationMode` on, and it already does the whole job:
 * `useCredentialFormController` loads the row through `useConfigurationDetail`,
 * seeds the form from it (`useFormSeeding`), renders the linked
 * `ai_credentials` reference in the picker and a declared `integer` as a
 * numeric control (`SchemaField.classify`), and saves through
 * `useUpdateConfiguration` — `PUT /configurations/configuration/{project}/{id}`.
 * Nothing about that page had to change; only the destination of this call.
 *
 * The id arrives as `configuration.id`, which is a NUMBER on the wire
 * (`ConfigurationItem.id: number`) even though the card's prop declares
 * `string`. It is stringified here rather than at the call site, so a caller
 * cannot put a number into a URL parameter by accident.
 *
 * The `routeStack` breadcrumb is still written, and still points at the page
 * the browser actually goes to, so a future reader of that state finds a path
 * that resolves. It is not load-bearing for this navigation.
 */
import { useCallback, useMemo } from 'react';

import { useLocation, useNavigate } from '@tanstack/react-router';

export function useConfigurationNavigation() {
  const navigate = useNavigate();
  const { state } = useLocation();

  const locationState = useMemo(
    () =>
      (state as { routeStack?: Array<{ breadCrumb: string; pagePath: string }> } | null)
        ?.routeStack ||
      [],
    [state],
  );

  const navigateToConfiguration = useCallback(
    (configurationId: string | number) => {
      const credentialUid = String(configurationId);
      void navigate({
        to: '/settings/edit-configuration/$credential_uid',
        params: { credential_uid: credentialUid },
        state: {
          routeStack: [
            ...locationState,
            {
              breadCrumb: 'AI Configuration',
              pagePath: '/settings/model-configuration',
            },
            {
              breadCrumb: 'Configuration',
              pagePath: `/settings/edit-configuration/${credentialUid}`,
            },
          ],
        } as Record<string, unknown>,
      });
    },
    [navigate, locationState],
  );

  return { navigateToConfiguration };
}
