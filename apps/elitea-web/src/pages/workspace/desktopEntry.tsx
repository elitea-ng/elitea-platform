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
 *
 * Outside `desktop` the routes' `beforeLoad` already redirects to `/chat`;
 * the component does the same if it is ever mounted anyway, so the web has no
 * Workspace screen and no string for one in its initial catalogue.
 */
import { lazy, Suspense } from 'react';

import { Navigate } from '@tanstack/react-router';

const WorkspacesPage = /* @__PURE__ */ lazy(() => import('./WorkspacesPage'));
const WorkspaceSessionPage = /* @__PURE__ */ lazy(() => import('./WorkspaceSessionPage'));
const TroubleshootPage = /* @__PURE__ */ lazy(() => import('./TroubleshootPage'));

function NotAvailable(): React.JSX.Element {
  return <Navigate to="/chat" replace />;
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

/** Settings › Troubleshoot (the Doctor); nothing outside `desktop` (an unknown settings tab renders empty). */
export function TroubleshootEntry(): React.JSX.Element | null {
  if (import.meta.env.MODE !== 'desktop') return null;
  return (
    <Suspense fallback={null}>
      <TroubleshootPage />
    </Suspense>
  );
}
