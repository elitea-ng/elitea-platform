/**
 * Pure helpers behind `ConfigurationsPanel` — option building, the `<<>>`
 * value shapes the selects match on, and the public-project gating.
 *
 * Split out of that component only because the file passed the 400-line
 * budget; nothing else imports these.
 */
import { isPublicProject } from '@/entities/project';
import { getConfig } from '@/shared/config';

import { EMPTY_MODELS_RESPONSE, type ModelsApiResponse } from '../../api/ai-configuration/api';

/* ── hook helper: build select options from a flat config list ──────────── */

export function buildOptions(configs: readonly Record<string, unknown>[]): Array<{ value: string; label: string }> {
  return (configs ?? []).map((cfg) => {
    const name = (cfg.elitea_title as string) || (cfg.label as string) || (cfg.type as string) || '';
    return {
      value: `${String(name)}<<>>${String((cfg.project_id as string) ?? '')}`,
      label: String(name),
    };
  });
}

/** `${default_model_name}<<>>${default_model_project_id}` — matches the
 * `<<>>`-joined value shape `buildOptions` produces, so the Select can
 * find the currently-selected option by value equality. */
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

/** `useModelsQuery` resolves to `undefined` before the fetch settles —
 * mirrors the old app's inline default arg on `useListModelsQuery`'s
 * destructure (`ModelConfiguration.jsx:32-45`). A plain top-level helper
 * (rather than a `??`/default-destructure at each of the 6 call sites)
 * keeps `ConfigurationsPanel` itself under the complexity budget. */
export function withDefaultModels(data: ModelsApiResponse | undefined): ModelsApiResponse {
  return data ?? EMPTY_MODELS_RESPONSE;
}

