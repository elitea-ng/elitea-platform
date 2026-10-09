/**
 * The ONLY door into the Workspace view, and it is shut in every build but
 * `desktop`.
 *
 * `import.meta.env.MODE` is a literal at build time, so in the default web
 * build the `if` below is `if ("production" !== "desktop")`: the lazy
 * components after it are unreachable, their pure-annotated `lazy()` calls are
 * dropped, and the dynamic `import()` of the real pages goes with them — no
 * Workspace chunk is emitted at all. The check is read at render time (not
 * module load) so a test can switch it with `vi.stubEnv`.
 */
import { lazy, Suspense } from 'react';

import Alert from '@mui/material/Alert';

import { t } from '@/shared/i18n';

const WorkspacesPage = /* @__PURE__ */ lazy(() => import('./WorkspacesPage'));
const WorkspaceSessionPage = /* @__PURE__ */ lazy(() => import('./WorkspaceSessionPage'));

function NotAvailable(): React.JSX.Element {
  return <Alert severity="info">{t('workspace.desktopOnly', 'Workspaces are available in the Elitea desktop app.')}</Alert>;
}

export function WorkspacesEntry(): React.JSX.Element {
  if (import.meta.env.MODE !== 'desktop') return <NotAvailable />;
  return (
    <Suspense fallback={null}>
      <WorkspacesPage />
    </Suspense>
  );
}

export function WorkspaceSessionEntry(): React.JSX.Element {
  if (import.meta.env.MODE !== 'desktop') return <NotAvailable />;
  return (
    <Suspense fallback={null}>
      <WorkspaceSessionPage />
    </Suspense>
  );
}
