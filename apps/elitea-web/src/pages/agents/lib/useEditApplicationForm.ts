import { useCallback, useMemo } from 'react';

import { zodResolver } from '@hookform/resolvers/zod';
import { useForm } from 'react-hook-form';

import { applicationEditSchema, type ApplicationCreationInput } from '@/entities/application-form';
import { applicationWriteHooks } from '@/features/agents';
import type { ApplicationDetail, ApplicationVersionDetail } from '@/shared/api/generated/model';

import { EMPTY_FORM_VALUES, toFormValues, toVersionSaveBody } from './editApplicationMappers';
import type { EditApplicationVersionFieldsState } from './useEditApplicationVersionFields';

export interface EditApplicationFormState {
  readonly form: ReturnType<typeof useForm<ApplicationCreationInput>>;
  readonly handleSave: () => void;
  readonly isSaving: boolean;
  /**
   * The most recent save attempt's failure, if any — old app:
   * `useSaveVersion.js:113-116`'s `if (error) { toastError(buildErrorMessage(error)); return false; }`,
   * called from `SaveApplicationButton.jsx`'s `handleSave` on every failed
   * save (network error, validation error, permission error, conflict).
   * This app has no toast infrastructure (same disclosed gap
   * `SaveToolkitButton.tsx`/`SaveNewVersionButton.tsx` already establish);
   * `useSaveVersion`'s own `error` state is threaded straight through
   * instead, so the caller (`EditApplication.tsx`) can render an inline
   * `role="alert"` banner — the same pattern that file already uses for its
   * detail-fetch error. Cleared automatically at the start of the next save
   * attempt (`useSaveVersion`'s own `setError(undefined)`). Covers a failure
   * of EITHER of the two calls that hook issues.
   */
  readonly saveError: unknown;
  /**
   * "Are there unsaved edits?", across BOTH halves of this page's state —
   * RHF's `formState.isDirty` (name/description) OR
   * `useEditApplicationVersionFields`' own comparison (instructions, welcome
   * message, variables, step limit). Combined here rather than at the call
   * site so the page keeps one source of truth for it, and so
   * `EditApplication` stays inside its §3.5 cyclomatic budget (12).
   */
  readonly isDirty: boolean;
}

/**
 * Split out of `EditApplication.tsx` for the same complexity/line-count
 * budget reasons as `useEditApplicationData` (this same `lib/` directory)
 * — owns the RHF instance, the imperative save action, and their shared
 * dependency on `activeVersion`.
 */
export function useEditApplicationForm(
  detail: ApplicationDetail | undefined,
  activeVersion: ApplicationVersionDetail | undefined,
  projectId: string | undefined,
  applicationId: number | undefined,
  versionFields: EditApplicationVersionFieldsState,
): EditApplicationFormState {
  const defaultValues = useMemo<ApplicationCreationInput>(
    () => (detail ? toFormValues(detail, activeVersion) : EMPTY_FORM_VALUES),
    [detail, activeVersion],
  );

  const form = useForm<ApplicationCreationInput>({
    resolver: zodResolver(applicationEditSchema),
    mode: 'onChange',
    values: defaultValues,
  });

  const versionId = activeVersion ? Number(activeVersion.id) : undefined;
  /*
   * #307 — this used to call `entities/application-form`'s
   * `useSaveApplicationVersion`, which issues the version PUT alone and,
   * at the time, had no `welcome_message` key in `ApplicationVersionDraft`
   * (the draft carries one now, for the CREATE paths).
   * The page therefore sent `conversation_starters` and nothing else: a
   * user could edit the name, the instructions or the welcome message, watch
   * the Save button succeed, and lose all of it.
   *
   * `features/agents`' `useSaveVersion` is the already-built, already-
   * exported hook for exactly this: it issues BOTH real calls the baseline's
   * single combined PUT used to fake — `updateApplicationVersion` for the
   * version's mutable fields and `editApplication` for the application-level
   * `name`/`description` — and takes a raw `VersionWriteRequest`, so
   * `welcome_message`, `meta` and (since #345) `tags` reach the wire. It
   * had zero production callers before this change; see its own module doc
   * for the endpoint split.
   *
   * #345 reaches it through `applicationWriteHooks` rather than through its
   * own barrel export — the repack that freed the slot `AgentTagEditor`
   * needed.
   */
  const { onSave, isSaving, error: saveError } = applicationWriteHooks.useSaveVersion();

  const handleSave = useCallback(() => {
    void form.handleSubmit(async (values) => {
      if (activeVersion === undefined || projectId === undefined || applicationId === undefined || versionId === undefined) {
        return;
      }
      const conversationStarters = (values.version_details?.conversation_starters ?? []).filter(
        (entry): entry is string => typeof entry === 'string',
      );
      const saved = await onSave({
        projectId,
        applicationId,
        versionId,
        version: toVersionSaveBody(activeVersion, conversationStarters, versionFields.fields),
        applicationName: values.name,
        applicationDescription: values.description,
      });
      /*
       * #133 — the page now arms the app-wide unsaved-changes guard off
       * `formState.isDirty`, so a successful save MUST clear that dirtiness
       * or the very next nav-away is prompted about changes already
       * persisted. `useSaveVersion` deliberately invalidates no GET-side
       * cache (see its own doc comment), so the `values` prop feeding
       * `useForm` below does not change on its own and RHF has no other
       * reason to reset. Reset to the values just submitted — NOT to the
       * server's echo, which carries only the version-level fields that
       * endpoint accepts and would blank out the name/description the page
       * still shows. Left dirty on failure, which is correct: the edits
       * really are still unsaved.
       *
       * #307 — `markSaved` is the same clearing for the version-level
       * fields, which live outside the RHF form and so are invisible to
       * `form.reset` (`useEditApplicationVersionFields`).
       */
      if (saved !== undefined) {
        form.reset(form.getValues());
        versionFields.markSaved();
      }
    })();
  }, [form, onSave, activeVersion, projectId, applicationId, versionId, versionFields]);

  return { form, handleSave, isSaving, saveError, isDirty: form.formState.isDirty || versionFields.isDirty };
}
