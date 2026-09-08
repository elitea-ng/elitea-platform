/**
 * Create one evaluation dataset.
 *
 * Only CREATE, deliberately. The reference's dialog also renames, and the
 * rename route exists — but a rename control with no way to reach it from this
 * slice's list would be a control nobody can press. It arrives with the list
 * row menu.
 *
 * `application_id` is NOT a field. A dataset opened from an agent's editor
 * belongs to that agent, and a dataset opened with no agent is project-wide;
 * offering the choice would let a person file an agent's cases under another
 * agent from a screen that never names it.
 */
import { useEffect, useState, type ReactNode } from 'react';

import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { EvalDatasetWriteRequest } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import { datasetErrorMessage } from '../lib/evaluationError';

export interface DatasetCreateDialogProps {
  readonly open: boolean;
  readonly applicationId: number | undefined;
  readonly isSaving: boolean;
  readonly error: unknown;
  readonly onClose: () => void;
  readonly onSubmit: (input: EvalDatasetWriteRequest) => void;
}

export function DatasetCreateDialog(props: DatasetCreateDialogProps): ReactNode {
  const { open, applicationId, isSaving, error, onClose, onSubmit } = props;
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');

  // Re-seeding on open is what makes "type a name, cancel, reopen" show an
  // empty form rather than the abandoned draft.
  useEffect(() => {
    if (open) {
      setName('');
      setDescription('');
    }
  }, [open]);

  const trimmedName = name.trim();
  const validationError =
    trimmedName === ''
      ? t('features.agentEvaluation.datasets.nameRequired', 'A dataset needs a name.')
      : undefined;
  // A failed save wins over the standing validation message: it is the newer
  // and more specific answer.
  const displayedError = datasetErrorMessage(error) ?? validationError;

  return (
    <BaseModal
      open={open}
      variant="simple"
      data-testid="dataset-create-dialog"
      title={t('features.agentEvaluation.datasets.createTitle', 'New dataset')}
      onClose={onClose}
      onConfirm={() => {
        if (validationError !== undefined) return;
        onSubmit({
          name: trimmedName,
          description: description.trim(),
          application_id: applicationId ?? null,
          is_shared: false,
        });
      }}
      actions={{
        confirmText: t('features.agentEvaluation.datasets.create', 'Create'),
        confirming: isSaving,
      }}
      content={
        <>
          <TextField
            fullWidth
            label={t('features.agentEvaluation.datasets.nameLabel', 'Name')}
            value={name}
            slotProps={{ htmlInput: { maxLength: 128, 'data-testid': 'dataset-name-input' } }}
            onChange={(event) => setName(event.target.value)}
          />
          <TextField
            fullWidth
            multiline
            label={t('features.agentEvaluation.datasets.descriptionLabel', 'Description')}
            value={description}
            slotProps={{ htmlInput: { 'data-testid': 'dataset-description-input' } }}
            onChange={(event) => setDescription(event.target.value)}
          />
          {displayedError !== undefined && (
            <Typography role="alert" variant="body2" color="error" data-testid="dataset-create-error">
              {displayedError}
            </Typography>
          )}
        </>
      }
    />
  );
}
