/**
 * Create / edit one evaluation dimension.
 *
 * Ported from the baseline's
 * `apps/elitea-ui/src/[fsd]/widgets/evaluation/ui/library/
 * DimensionEditorDialog.jsx`, minus the two things that belong to a BINDING
 * rather than to a dimension:
 *
 *   - the `evidence_scope` toggles, which describe what a binding shows a
 *     judge. Bindings do not exist in this release, and the server refuses an
 *     `evidence_scope` field outright rather than accepting and discarding it.
 *   - the platform tier, which the baseline never offered here either: those
 *     rows are materialised from a platform catalogue this release does not
 *     serve.
 */
import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';

import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import { toFormState, toggleEngine, toWriteInput, validateDimensionForm } from '../lib/dimensionForm';
import { useEvalDimensionMutations } from '../model/useEvalDimensions';
import type { EvalDimension, EvalDimensionForm, EvalEngine } from '../model/types';
import { DimensionEditorFields } from './DimensionEditorFields';

export interface DimensionEditorDialogProps {
  readonly open: boolean;
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  /** `undefined` opens the dialog in create mode. */
  readonly dimension: EvalDimension | undefined;
  /** The caller holds dimension.create: an agent dimension may move to the project library. */
  readonly canPromote: boolean;
  readonly onClose: () => void;
}

function saveErrorMessage(error: unknown): string {
  const fallback = t('features.agentEvaluation.saveFailed', 'Failed to save the dimension.');
  if (error instanceof Error && error.message !== '') return error.message;
  return fallback;
}

export function DimensionEditorDialog(props: DimensionEditorDialogProps): ReactNode {
  const { open, projectId, applicationId, dimension, canPromote, onClose } = props;
  const isEdit = dimension !== undefined;

  const [form, setForm] = useState<EvalDimensionForm>(() => toFormState(dimension));
  const [submitError, setSubmitError] = useState<string | undefined>(undefined);

  const mutations = useEvalDimensionMutations(projectId);
  const isSaving = mutations.create.isPending || mutations.update.isPending;

  // Re-seeding on open is what makes "edit A, cancel, edit B" show B rather
  // than the abandoned edits of A. Keyed on `open` as well as `dimension` so a
  // reopen of the SAME row also discards the cancelled draft.
  useEffect(() => {
    if (open) {
      setForm(toFormState(dimension));
      setSubmitError(undefined);
    }
  }, [open, dimension]);

  const setField = useCallback(
    <K extends keyof EvalDimensionForm>(key: K, value: EvalDimensionForm[K]): void => {
      setForm((previous) => ({ ...previous, [key]: value }));
    },
    [],
  );

  const handleToggleEngine = useCallback((engine: EvalEngine): void => {
    setForm((previous) => ({
      ...previous,
      allowed_engines: toggleEngine(previous.allowed_engines, engine),
    }));
  }, []);

  const validationError = useMemo(() => validateDimensionForm(form, applicationId), [form, applicationId]);

  /*
   * The blocking reason is shown BEFORE a save is attempted, not only after.
   * A Confirm that quietly does nothing is how "polarity is required" reads as
   * a broken dialog. A failed save (`submitError`) wins over the standing
   * validation message, because it is the newer and more specific answer.
   */
  const displayedError =
    submitError ?? (validationError ? t(validationError.key, validationError.text) : undefined);

  const handleSave = useCallback((): void => {
    if (validationError) {
      setSubmitError(t(validationError.key, validationError.text));
      return;
    }
    setSubmitError(undefined);
    const input = toWriteInput(form, applicationId);
    const request =
      isEdit && dimension
        ? mutations.update.mutateAsync({ dimensionId: dimension.id, input })
        : mutations.create.mutateAsync(input);
    request.then(
      () => onClose(),
      (error: unknown) => setSubmitError(saveErrorMessage(error)),
    );
  }, [validationError, form, applicationId, isEdit, dimension, mutations, onClose]);

  return (
    <BaseModal
      open={open}
      variant="complex"
      data-testid="dimension-editor-dialog"
      title={
        isEdit
          ? t('features.agentEvaluation.editTitle', 'Edit dimension')
          : t('features.agentEvaluation.createTitle', 'New dimension')
      }
      onClose={onClose}
      onConfirm={handleSave}
      actions={{
        confirmText: isEdit
          ? t('features.agentEvaluation.save', 'Save')
          : t('features.agentEvaluation.create', 'Create'),
        confirming: isSaving,
      }}
      content={
        <>
          <DimensionEditorFields
            form={form}
            isEdit={isEdit}
            canScopeToAgent={applicationId !== undefined}
            canPromote={canPromote}
            storedTier={dimension?.tier}
            onFieldChange={setField}
            onToggleEngine={handleToggleEngine}
          />
          {displayedError !== undefined && (
            <Typography
              role="alert"
              variant="bodyMedium"
              component="p"
              color="error"
              data-testid="dimension-editor-error"
            >
              {displayedError}
            </Typography>
          )}
        </>
      }
    />
  );
}
