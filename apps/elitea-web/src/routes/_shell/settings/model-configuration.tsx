/**
 * ROUTE-053 `/settings/model-configuration` -> `AIConfiguration`.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/pages/settings/AIConfiguration.jsx`.
 *
 * Changes from the baseline:
 * - Replaced `StickyTabs` with `BaseTab` + `BaseTabs` (R-UI-T2).
 * - Tour targets (`AI_CONFIG_TOUR_TARGET_IDS`) are dropped (tour system
 *   not ported in Wave-2/unit-A9).
 * - The old page rendered its own tab bar; the new layout is provided by
 *   `settings-layout.tsx` which renders `SettingsDrawer` + `<Outlet />`.
 *   This page is ONE of the two tab pages (AI Configuration / OpenAI Template).
 *   The real tab bar lives in `settings-layout.tsx`'s drawer.
 * - This component itself is a self-contained tab content renderer that
 *   hosts its own sub-tabs (Model Configuration / OpenAI Template),
 *   matching the old app's two-tab inner structure.
 *
 * Phase 2: the `RouteShell` heading that used to sit above `AIConfiguration`
 * is removed. It emitted an `<h1>AI Configuration</h1>` that production does
 * not have — checked against the captured baseline
 * (`parity/screenshot-index.json`, `settings-ai-configuration.dark.jpeg`):
 * the content pane starts directly with this page's own
 * "AI Configuration"/"OpenAI Template" sub-tabs, and the settings drawer
 * already carries the label. No replacement header is added, because adding
 * one would introduce a title the baseline never shows.
 */
import { createFileRoute } from '@tanstack/react-router';

import { pickParams } from '@/routes/-search/params';
import { RouteError, RoutePending } from '@/routes/-ui/RouteStatus';

import { AIConfiguration } from '@/pages/settings/AIConfiguration';
import { useSelectedProjectStore } from '@/widgets/app-shell';

export const Route = createFileRoute('/_shell/settings/model-configuration')({
  // `reveal` (PARAM group "settings"): the just-saved/edited configuration's
  // id, so `AIConfiguration`/`ConfigurationsPanel` can open the section it
  // actually lives in on return from `/settings/create-configuration` or
  // `/settings/edit-configuration/$credential_uid` — see those routes' `leave`.
  validateSearch: pickParams('reveal'),
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: ModelConfigurationShell,
});

function ModelConfigurationShell() {
  const projectId = useSelectedProjectStore((s) => s.project?.id ?? '');
  const { reveal } = Route.useSearch();

  return (
    <AIConfiguration
      projectId={projectId}
      {...(reveal !== '' ? { revealConfigurationId: reveal } : {})}
    />
  );
}
