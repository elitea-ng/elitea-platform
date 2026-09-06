/**
 * ROUTE-052 `/settings` (index) -> redirect to the default tab, `replace`.
 *
 * The target is `DEFAULT_SETTINGS_TAB` ("project-general"), not the
 * hard-coded `model-configuration` this route used to name. The reference's
 * `DEFAULT_TAB` is `project-general` and a live deployment opens there; two
 * places decide this (here and `SettingsRedirect`), so they read one
 * constant rather than each carrying their own literal.
 */
import { createFileRoute, redirect } from '@tanstack/react-router';

import { DEFAULT_SETTINGS_TAB } from '@/shared/ui/settings/SettingsRedirect';

export const Route = createFileRoute('/_shell/settings/')({
  beforeLoad: () => {
    // oxlint-disable-next-line typescript/only-throw-error -- TanStack Router's beforeLoad redirect contract: throw the Response redirect() returns, not an Error (verified against the installed @tanstack/router-core's own redirect() implementation).
    throw redirect({ to: '/settings/$tab', params: { tab: DEFAULT_SETTINGS_TAB }, replace: true });
  },
});
