import { useCallback, useMemo, useState } from 'react';

import { zodResolver } from '@hookform/resolvers/zod';
import { useForm } from 'react-hook-form';

import { applicationCreationSchema, useSaveApplicationVersion, type ApplicationCreationInput } from '@/entities/application-form';
import { usePipelineGraphDraft } from '@/features/pipelines';
import type { ApplicationDetail, ApplicationVersionDetail } from '@/shared/api/generated/model';

import { EMPTY_FORM_VALUES, toFormValues, toVersionDraft } from './editPipelineMappers';
import { useEditPipelineLlmSettings, type EditPipelineLlmSettingsState } from './useEditPipelineLlmSettings';

export interface EditPipelineFormState {
  readonly form: ReturnType<typeof useForm<ApplicationCreationInput>>;
  readonly handleSave: () => void;
  readonly isSaving: boolean;
  /**
   * The most recent save attempt's failure, if any — old app:
   * `useSaveVersion.js:113-116`'s `if (error) { toastError(buildErrorMessage(error)); return false; }`,
   * called from `SaveApplicationButton.jsx`'s `handleSave` on every failed
   * save (network error, validation error, permission error, conflict).
   * Reproduced verbatim from `pages/agents/lib/useEditApplicationForm.ts`'s
   * own `saveError` (Wave-2 unit A1g) — this app has no toast
   * infrastructure (same disclosed gap `SaveToolkitButton.tsx`/
   * `SaveNewVersionButton.tsx` already establish); `useSaveApplicationVersion`'s
   * own `error` state is threaded straight through instead, so the caller
   * (`EditPipeline.tsx`) can render an inline `role="alert"` banner — the
   * same pattern that file already uses for its detail-fetch error.
   * Adversarial-review fix: previously discarded entirely, so a failed save
   * gave the user zero feedback. Cleared automatically at the start of the
   * next save attempt (`useSaveApplicationVersion`'s own `setError(undefined)`).
   */
  readonly saveError: unknown;
  /**
   * The model this version runs on, as the page's picker edits it. Owned here
   * rather than by the page because the save body reads it and because
   * `isDirty` below has to include it — a picked model the nav blocker cannot
   * see is a model the user loses by navigating away (#133).
   */
  readonly llmSettings: EditPipelineLlmSettingsState;
  /** "Are there unsaved edits?" across both halves of this page's form state — RHF's `formState.isDirty` (name/description) or a changed model. The flow editor's own YAML dirtiness is the page's third half. */
  readonly isDirty: boolean;
  /**
   * `true` when the last Save click was refused because the live graph is one
   * the native runtime would not accept.
   *
   * The graph veto is published by `features/pipelines`' `GraphAdmissionGate`
   * as an RHF `root.*` error, which disables the Save button — and that was
   * its ONLY enforcement. `form.handleSubmit` deletes every `root.*` error
   * before deciding whether to submit (react-hook-form 7.83:
   * `_formState.errors = errors` from the resolver, then
   * `unset(_formState.errors, 'root')`, `dist/index.esm.mjs:2989/3002`), so
   * the submit path itself had no admission check at all. That is reachable,
   * not theoretical: `useRefetchPipelineAfterSave` now fires a detail refetch
   * after every save, and a failed one puts `ConfigurationTab` into its error
   * branch, which unmounts the gate — while `EditPipeline` keeps the Save bar
   * mounted with `canSave = isValid && !isSaving`, now true. One click would
   * have PUT the inadmissible document.
   */
  readonly admissionRefused: boolean;
}

/**
 * Split out of `EditPipeline.tsx` for the same complexity/line-count budget
 * reasons as `useEditPipelineData` (this same `lib/` directory) — owns the
 * RHF instance, the imperative save action, and their shared dependency on
 * `activeVersion`. Same shape as `pages/agents/lib/useEditApplicationForm.ts`
 * (Wave-2 unit A1g) — `useSaveApplicationVersion` is the same promoted
 * `entities/application-form` mutation both domains share (a Pipeline
 * literally IS an Application row).
 */
export function useEditPipelineForm(
  detail: ApplicationDetail | undefined,
  activeVersion: ApplicationVersionDetail | undefined,
  projectId: string | undefined,
  applicationId: number | undefined,
): EditPipelineFormState {
  const defaultValues = useMemo<ApplicationCreationInput>(
    () => (detail ? toFormValues(detail, activeVersion) : EMPTY_FORM_VALUES),
    [detail, activeVersion],
  );

  const form = useForm<ApplicationCreationInput>({
    resolver: zodResolver(applicationCreationSchema),
    mode: 'onChange',
    values: defaultValues,
  });

  const versionId = activeVersion ? Number(activeVersion.id) : undefined;
  const { save, isSaving, error: saveError } = useSaveApplicationVersion(projectId, applicationId, versionId);
  // #135 (write half): read the flow editor's LIVE graph at click time. This
  // used to submit `toVersionDraft(activeVersion, conversationStarters)` and
  // nothing else — no nodes, no edges, no `pipeline_settings` — so the PUT
  // answered 200 and every graph edit was gone on the next reload.
  const readGraphDraft = usePipelineGraphDraft();
  const llmSettings = useEditPipelineLlmSettings(activeVersion);
  const [admissionRefused, setAdmissionRefused] = useState(false);

  const handleSave = useCallback(() => {
    void form.handleSubmit(async (values) => {
      if (activeVersion === undefined) return;
      // The save path's OWN admission check — see `admissionRefused` above for
      // why the disabled button is not enough. Judged on the exact string
      // about to be stored (`PipelineGraphDraft.admission`), not on the
      // canvas's last good parse.
      const graph = readGraphDraft();
      if (graph !== undefined && !graph.admission.isAdmissible) {
        setAdmissionRefused(true);
        return;
      }
      setAdmissionRefused(false);
      const conversationStarters = (values.version_details?.conversation_starters ?? []).filter(
        (entry): entry is string => typeof entry === 'string',
      );
      // The 4th argument is the live pick; `toVersionDraft` falls back to the
      // stored blob when it is `undefined`, so a version nobody re-pointed
      // keeps whatever model it already named — including none at all, which
      // is what leaves the catalogue-default fallback in charge.
      const tags = values.version_details?.tags ?? [];
      const saved = await save(toVersionDraft(activeVersion, conversationStarters, graph, llmSettings.value, tags));
      /*
       * #133 — the page now arms the app-wide unsaved-changes guard off
       * `formState.isDirty`, so a successful save must clear that dirtiness
       * or the next nav-away is prompted about changes already persisted.
       * `useSaveApplicationVersion` invalidates no GET-side cache by design
       * (its own doc comment), so the `values` prop feeding `useForm` above
       * never changes and RHF has no other reason to reset. Left dirty on
       * failure — those edits really are still unsaved.
       */
      if (saved !== undefined) {
        form.reset(form.getValues());
        // The same clearing for the model, which lives outside the RHF form
        // and so is invisible to `form.reset`.
        llmSettings.markSaved();
      }
    })();
  }, [form, save, activeVersion, readGraphDraft, llmSettings]);

  return {
    form,
    handleSave,
    isSaving,
    saveError,
    llmSettings,
    isDirty: form.formState.isDirty || llmSettings.isDirty,
    admissionRefused,
  };
}
