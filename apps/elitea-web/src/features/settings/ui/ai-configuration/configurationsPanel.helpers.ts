/**
 * Pure helpers behind `ConfigurationsPanel` — the `<<>>` value shapes the
 * selects match on, and the public-project gating.
 *
 * Split out of that component only because the file passed the 400-line
 * budget; nothing else imports these.
 *
 * The option BUILDER that used to live here is gone (#80). It read the
 * CONFIGURATION rows and labelled each with `elitea_title`, which no
 * `defaultValueOf` below can ever equal — that value is a MODEL name. The
 * options now come from the model catalogue, through
 * `../../lib/ai-configuration/useModelOptions`, which is what the baseline
 * always read.
 */
import { isPublicProject } from '@/entities/project';
import { getConfig } from '@/shared/config';

import type { ModelsApiResponse } from '../../api/ai-configuration/api';

/** `${default_model_name}<<>>${default_model_project_id}` — matches the
 * `<<>>`-joined value shape `useModelOptions`' `createOptions` produces, so
 * the Select can find the currently-selected option by value equality. */
export function defaultValueOf(data: ModelsApiResponse): string {
  return `${data.default_model_name ?? ''}<<>>${data.default_model_project_id ?? ''}`;
}

/** Low/high-tier defaults are optional — old app guards both halves being
 * present before building the `<<>>` value (`ModelConfiguration.jsx:168-180`),
 * otherwise leaves the Select unset rather than showing `"<<>>"`. */
export function tierDefaultValueOf(name: string | undefined, tierProjectId: string | undefined): string {
  if (!name || !tierProjectId) return '';
  return `${name}<<>>${tierProjectId}`;
}

/** Tenant/public-project gating for the "Create configuration" button —
 * old app: `ALLOW_PROJECT_OWN_LLMS !== false || projectId == PUBLIC_PROJECT_ID`
 * (`ConfigurationsPanel.jsx:36-39`). Also drives `include_shared` for the
 * model-defaults fetch, matching `useListModelsQuery`'s
 * `include_shared: projectId != PUBLIC_PROJECT_ID`. Extracted to a
 * top-level function (rather than an inline `useMemo` callback) to keep
 * `ConfigurationsPanel` itself under the complexity budget. */
export function computeProjectGating(projectId: string): { includeShared: boolean; canCreateConfiguration: boolean } {
  const result = getConfig();
  if (result.status !== 'ok') {
    // Defensive fallback only — `app/App.tsx` renders `MissingEnvPage`
    // before this ever mounts in production (see `integrationGuard.ts`'s
    // identical posture on this branch).
    return { includeShared: true, canCreateConfiguration: true };
  }
  const isPublic = isPublicProject(projectId, result.config.vite_public_project_id);
  return {
    includeShared: !isPublic,
    canCreateConfiguration: result.config.allow_project_own_llms !== false || isPublic,
  };
}

/** Whether one of `configs` is the row named by `revealConfigurationId` (the
 * `?reveal=` search param — see `ConfigurationsPanel.tsx`'s prop doc comment).
 * `undefined` (no reveal in play) never matches. A top-level helper, not an
 * inline `.some()` at each of the 6 non-LLM call sites, for the same
 * complexity-budget reason as this file's other helpers. */
export function sectionHoldsRevealedRow(configs: readonly Record<string, unknown>[], revealConfigurationId: string | undefined): boolean {
  if (revealConfigurationId === undefined) return false;
  return configs.some((c) => String(c['id']) === revealConfigurationId);
}
