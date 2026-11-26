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

import { isDesktopSettingsTab, type DesktopSettingsTab } from '@/shared/ui/settings/desktopSettingsTabs';

const WorkspacesPage = /* @__PURE__ */ lazy(() => import('./WorkspacesPage'));
const WorkspaceSessionPage = /* @__PURE__ */ lazy(() => import('./WorkspaceSessionPage'));
const TroubleshootPage = /* @__PURE__ */ lazy(() => import('./TroubleshootPage'));
const RulesPage = /* @__PURE__ */ lazy(() => import('./RulesPage'));
const McpPage = /* @__PURE__ */ lazy(() => import('./McpPage'));
const HooksPage = /* @__PURE__ */ lazy(() => import('./HooksPage'));
const ComputerUsePage = /* @__PURE__ */ lazy(() => import('./ComputerUsePage'));
const TrustedFoldersPage = /* @__PURE__ */ lazy(() => import('./TrustedFoldersPage'));

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
function TroubleshootEntry(): React.JSX.Element | null {
  if (import.meta.env.MODE !== 'desktop') return null;
  return (
    <Suspense fallback={null}>
      <TroubleshootPage />
    </Suspense>
  );
}

/** Settings › Approval rules (#1216); nothing outside `desktop`. */
function RulesEntry(): React.JSX.Element | null {
  if (import.meta.env.MODE !== 'desktop') return null;
  return (
    <Suspense fallback={null}>
      <RulesPage />
    </Suspense>
  );
}

/** Settings › MCP servers (#1230); nothing outside `desktop`. */
function McpEntry(): React.JSX.Element | null {
  if (import.meta.env.MODE !== 'desktop') return null;
  return (
    <Suspense fallback={null}>
      <McpPage />
    </Suspense>
  );
}

/** Settings › Hooks (#1237); nothing outside `desktop`. */
function HooksEntry(): React.JSX.Element | null {
  if (import.meta.env.MODE !== 'desktop') return null;
  return (
    <Suspense fallback={null}>
      <HooksPage />
    </Suspense>
  );
}

/** Settings › Computer use (ADR-0047 decision 7, #1584); nothing outside `desktop`. */
function ComputerUseEntry(): React.JSX.Element | null {
  if (import.meta.env.MODE !== 'desktop') return null;
  return (
    <Suspense fallback={null}>
      <ComputerUsePage />
    </Suspense>
  );
}

/** Settings › Trusted folders (ADR-0061 decision 5i, #3062); nothing outside `desktop`. */
function TrustedFoldersEntry(): React.JSX.Element | null {
  if (import.meta.env.MODE !== 'desktop') return null;
  return (
    <Suspense fallback={null}>
      <TrustedFoldersPage />
    </Suspense>
  );
}

const DESKTOP_SETTINGS_ENTRIES: Record<DesktopSettingsTab, () => React.JSX.Element | null> = {
  'approval-rules': RulesEntry,
  'mcp-servers': McpEntry,
  hooks: HooksEntry,
  'computer-use': ComputerUseEntry,
  'trusted-folders': TrustedFoldersEntry,
  troubleshoot: TroubleshootEntry,
};

/**
 * The desktop Settings tab `tab` (`DESKTOP_SETTINGS_TABS`, #3078), as the `/settings/$tab` catch-all renders it;
 * nothing for any other slug, and nothing outside `desktop`.
 */
export function DesktopSettingsEntry({ tab }: { tab: string }): React.JSX.Element | null {
  if (!isDesktopSettingsTab(tab)) return null;
  const Entry = DESKTOP_SETTINGS_ENTRIES[tab];
  return <Entry />;
}
