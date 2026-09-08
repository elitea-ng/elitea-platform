import { useCallback, useMemo, useState } from 'react';

import { zodResolver } from '@hookform/resolvers/zod';
import { useForm } from 'react-hook-form';

import { applicationCreationSchema, type ApplicationCreationInput } from '@/entities/application-form';
import { applicationWriteHooks } from '@/features/agents';
import { usePipelineGraphDraft } from '@/features/pipelines';
import type { ApplicationDetail, ApplicationVersionDetail } from '@/shared/api/generated/model';

import { EMPTY_FORM_VALUES, toFormValues, toPipelineVersionSaveBody } from './editPipelineMappers';
import type { EditPipelineVersionFieldsState } from './useEditPipelineVersionFields';

export interface EditPipelineFormState {
  readonly form: ReturnType<typeof useForm<ApplicationCreationInput>>;
  readonly handleSave: () => void;
  readonly isSaving: boolean;
  /**
   * The most recent save attempt's failure, if any — old app:
   * `useSaveVersion.js:113-116`'s `if (error) { toastError(...); return false; }`.
   * This app has no toast infrastructure, so the hook's own `error` state is
   * threaded straight through and the caller renders an inline `role="alert"`
   * banner. Covers a failure of EITHER of the two calls the save issues.
   */
  readonly saveError: unknown;
  /** "Are there unsaved edits?" across both halves of this page's form state — RHF's `formState.isDirty` (name/description/starters) or a changed version-level field. The flow editor's own YAML dirtiness is the page's third half. */
  readonly isDirty: boolean;
  /**
   * `true` when the last Save click was refused because the live graph is one
   * the native runtime would not accept.
   *
   * The graph veto is published by `features/pipelines`' `GraphAdmissionGate`
   * as an RHF `root.*` error, which disables the Save button — and that was
   * its ONLY enforcement. `form.handleSubmit` deletes every `root.*` error
   * before deciding whether to submit (react-hook-form 7.83,
   * `dist/index.esm.mjs:2989/3002`), so the submit path itself had no
   * admission check at all.
   */
  readonly admissionRefused: boolean;
}

/**
 * Split out of `EditPipeline.tsx` for the same complexity/line-count budget
 * reasons as `useEditPipelineData` — owns the RHF instance, the imperative
 * save action, and their shared dependency on `activeVersion`.
 *
 * **The save endpoint changed, and the reason is a measured data loss.** This
 * hook used to call `entities/application-form`'s `useSaveApplicationVersion`,
 * which issues the VERSION PUT alone. The pipeline editor's name and
 * description are APPLICATION-level columns (`applications.name`/
 * `.description`), written only by `PUT /elitea_core/application/prompt_lib/
 * {projectId}/{applicationId}` — so every rename a person typed into this
 * page was validated, marked dirty, submitted, answered 200, and discarded.
 * `features/agents`' `useSaveVersion` is the already-built hook that issues
 * BOTH real calls, and it takes a raw `VersionWriteRequest`, which is also
 * what lets `tags` and `welcome_message` reach the wire
 * (`toVersionWriteRequest`, the draft mapper the old path used, writes no
 * `tags` key at all). `pages/agents/lib/useEditApplicationForm.ts` made the
 * identical move for the identical reason.
 */
export function useEditPipelineForm(
  detail: ApplicationDetail | undefined,
  activeVersion: ApplicationVersionDetail | undefined,
  projectId: string | undefined,
  applicationId: number | undefined,
  versionFields: EditPipelineVersionFieldsState,
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
  const { onSave, isSaving, error: saveError } = applicationWriteHooks.useSaveVersion();
  // #135 (write half): read the flow editor's LIVE graph at click time, so the
  // PUT carries the nodes, edges and `pipeline_settings` geometry the canvas
  // currently shows rather than the ones the server last sent.
  const readGraphDraft = usePipelineGraphDraft();
  const [admissionRefused, setAdmissionRefused] = useState(false);

  const handleSave = useCallback(() => {
    void form.handleSubmit(async (values) => {
      if (activeVersion === undefined || projectId === undefined || applicationId === undefined || versionId === undefined) {
        return;
      }
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
      const saved = await onSave({
        projectId,
        applicationId,
        versionId,
        version: toPipelineVersionSaveBody(activeVersion, conversationStarters, versionFields.fields, graph),
        applicationName: values.name,
        applicationDescription: values.description,
      });
      /*
       * #133 — the page arms the app-wide unsaved-changes guard off
       * `isDirty`, so a successful save must clear BOTH halves of it or the
       * next nav-away is prompted about changes already persisted.
       * `useSaveVersion` invalidates no GET-side cache by design, so the
       * `values` prop feeding `useForm` never changes and RHF has no other
       * reason to reset. Reset to the values just submitted, NOT to the
       * server's echo — that echo carries only the version-level fields and
       * would blank the name/description the page still shows. Left dirty on
       * failure, which is correct: those edits really are still unsaved.
       */
      if (saved !== undefined) {
        form.reset(form.getValues());
        versionFields.markSaved();
      }
    })();
  }, [form, onSave, activeVersion, projectId, applicationId, versionId, readGraphDraft, versionFields]);

  return {
    form,
    handleSave,
    isSaving,
    saveError,
    isDirty: form.formState.isDirty || versionFields.isDirty,
    admissionRefused,
  };
}
