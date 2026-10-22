import { useCallback } from 'react';

import type { ApplicationVersionDraft } from '@/entities/application-form';
import { useCreateApplicationDraft } from '@/entities/application-form';
import type { ApplicationCreatedResponse } from '@/shared/api/generated/model';

import { applicationErrorMessage } from '../lib/errorMessage';

/**
 * Port of `apps/elitea-ui/src/hooks/application/useCreateApplication.jsx`.
 *
 * **DISCLOSED REDESIGN — three things dropped, one thing moved to the
 * caller, all independently evidenced:**
 *
 * 1. **No Formik.** The baseline reads/writes `formik.values`/
 *    `formik.setFieldError`/`formik.resetForm` directly. This app has no
 *    Formik dependency (react-hook-form + zod — `package.json`); the
 *    established convention for this exact situation
 *    (`features/agents/model/types.ts`'s own "DISCLOSED REDESIGN" doc
 *    comment, `features/mcps/ui/McpAuthStatusBadge.tsx`) is a plain typed
 *    input object instead of ambient form context. `create()` below takes a
 *    `CreateApplicationInput` and returns the created row (or `undefined` on
 *    failure); the caller decides what to do with its own form state.
 *
 * 2. **No per-field error mapping.** The baseline's `error.data.forEach(({loc,
 *    msg}) => formik.setFieldError(loc[0], msg))` assumes the
 *    legacy backend's `[{loc, msg, ctx}]` pydantic validation-error
 *    array. The real Go handler for this endpoint
 *    (`services/elitea-main/internal/api/v2/applications/handler.go:345-479`,
 *    `Create`) responds with the flat `{"error": "message"}` shape on
 *    failure (`pkg/apierr`) — there is no per-field `loc` to key a
 *    `setFieldError` call off. `error` is exposed as-is (an `EliteaApiError`)
 *    plus `applicationErrorMessage(error)` for a flat display string; a
 *    caller that wants field-level attribution has no structured data to
 *    build it from today (a real, disclosed backend gap, not a shortcut).
 *
 * 3. **No pipeline-editor coupling.** The baseline computes
 *    `calculateNodesAndEdges(yamlCode, ...)` via `@/pages/Pipelines/
 *    useSavePipeline` (a `features/pipelines` internal) and reads
 *    `state.pipelineEditor.nodes` from Redux — both genuinely off-limits to
 *    `features/agents` (`no-sideways-features`; this app also has no Redux).
 *    Reading the real contract resolves this WITHOUT an injected callback:
 *    `ApplicationVersionDraft` (`entities/application-form/model/
 *    initialValues.ts`) already takes a plain `instructions: string` field —
 *    for a pipeline draft, the CALLER (a pipeline-editor component, which
 *    legitimately owns `useSavePipeline`-equivalent logic) is responsible
 *    for resolving `instructions` to the compiled YAML before constructing
 *    the draft passed in here; this hook never needs to know a pipeline
 *    exists. The one field that genuinely has nowhere to go is
 *    `pipeline_settings` (node/edge layout) — entities/application-form's
 *    own `ApplicationVersionDraft.pipelineSettings` doc comment already
 *    discloses that NEITHER `ApplicationCreateRequest` NOR `VersionWriteRequest`
 *    (checked directly against both `.zod.ts` files) carries any such field
 *    on ANY write endpoint — confirmed again by reading
 *    `services/elitea-main/internal/api/v2/applications/handler.go`'s `Create`
 *    handler directly: it reads `name`/`description`/`type`/`agent_type`/
 *    `instructions`/`welcome_message`/`llm_settings`/`conversation_starters`/
 *    `variables` off `versions[0]` and nothing else. There is no
 *    features/pipelines coupling left to resolve here because there is no
 *    transport for the one field that coupling used to exist for.
 *
 * 4. **No navigation, no nav-blocker reset.** The baseline calls
 *    `navigate(...)` and `resetBlockNav()` on success.
 *    `widgets/app-shell/model/navBlocker.store.ts`'s own doc comment already
 *    flags this exact layering gap: "`features/*` sits BELOW `widgets/**`...
 *    a `features/*` slice importing FROM `widgets/app-shell` would be an
 *    upward import `no-upward-from-features` rejects, [...] flagging this
 *    for whoever next touches the layer boundaries" — this unit is exactly
 *    that "whoever". `ApplicationCatalog.tsx` (`features/apps`) already
 *    established the same call for plain navigation: "dropped in favour of
 *    TanStack Router's typed `useNavigate`... [component] takes an
 *    `onConfigure` callback prop instead". `create()`'s `options.onSuccess`
 *    callback (kept, same name/shape as the baseline) is where a
 *    page-level caller — which CAN import both this hook and
 *    `useNavBlockerStore` — performs both.
 */

export interface CreateApplicationInput {
  readonly name: string;
  readonly description?: string;
  readonly icon?: string;
  readonly version: ApplicationVersionDraft;
}

export interface UseCreateApplicationOptions {
  /** Called with the normalised created row after a successful create — the caller's seam for navigation/nav-blocker/toast (see doc comment point 4). */
  readonly onSuccess?: (data: ApplicationCreatedResponse) => void;
}

export interface UseCreateApplicationResult {
  readonly create: (input: CreateApplicationInput) => Promise<ApplicationCreatedResponse | undefined>;
  readonly isLoading: boolean;
  readonly error: unknown;
  readonly errorMessage: string | undefined;
}

export function useCreateApplication(
  projectId: string | undefined,
  options: UseCreateApplicationOptions = {},
): UseCreateApplicationResult {
  const { onSuccess } = options;
  const { create: createDraft, isCreating, error } = useCreateApplicationDraft(projectId);

  const create = useCallback(
    async (input: CreateApplicationInput): Promise<ApplicationCreatedResponse | undefined> => {
      const trimmedName = input.name.trim();
      const created = await createDraft({
        name: trimmedName,
        ...(input.description !== undefined ? { description: input.description } : {}),
        type: 'interface',
        ...(input.icon !== undefined ? { icon: input.icon } : {}),
        version: input.version,
      });
      if (created === undefined) {
        return undefined;
      }
      onSuccess?.(created);
      return created;
    },
    [createDraft, onSuccess],
  );

  return {
    create,
    isLoading: isCreating,
    error,
    errorMessage: error === undefined ? undefined : applicationErrorMessage(error),
  };
}
