/**
 * Create / edit one platform-wide model.
 *
 * ## An edit MERGES over the stored row
 *
 * The update replaces the `data` column whole. This dialog used to build that
 * column out of the fields it shows, so every field it did not show was erased
 * by any save at all — an `llm_model` declares nine, and a rename reset the
 * model's context window, its output limit and its three capability flags to the
 * registry defaults, with a 200 and nothing on the screen that read the loss
 * back.
 *
 * The listing now carries the stored object and the submit writes the edited
 * fields OVER it (`platformModelForm.ts`), so a field this dialog has never
 * heard of survives an edit it was not part of.
 *
 * ## The credential is a required select, not a text field and not optional
 *
 * A model names its credential by TITLE, and the server refuses a title that is
 * not among the platform's published providers. Offering free text would make
 * that refusal the operator's first feedback on a name they typed correctly but
 * spelled differently — and the failure it guards against is a model advertised
 * to every project whose provider is guessed from a prefix in its name.
 *
 * "None — infer from the model name" used to be offered beside them, on the
 * argument that the gateway falls back to reading the provider out of a prefix.
 * It never described a row this dialog could write. The registry declares
 * `ai_credentials` required on all five model types, so a model with no link
 * fails provider admission and is stored with `status_ok = false` — and the
 * catalogue, the tier defaults and the gateway all select on `status_ok = true`.
 * The option produced a row that was listed HERE and served nowhere, and the
 * server now answers 400 for it. So the select has no empty value, and Save
 * waits for one.
 *
 * ## The type cannot change on an edit
 *
 * The server derives the row's `section` from its type, and the gateway matches
 * on the (section, type) pair. Retyping a stored model would need both columns
 * rewritten together; the partial update writes what it is sent, so the honest
 * path is to delete and re-create.
 *
 * ## The chat fields are only offered where the schema has them
 *
 * The tiers, the context window and the capabilities are `llm_model` fields. The
 * other four model types decode their `data` with unknown fields REFUSED, so
 * sending one they do not declare would turn a valid model into an invalid
 * binding — see PlatformModelChatFields.tsx.
 */
import type { ReactNode } from 'react';
import { useEffect, useState } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { PlatformModelChatFields } from './PlatformModelChatFields';
import {
  platformModelTypeLabel,
  type PlatformModel,
  type PlatformModelDraft,
} from './api/adminLlmPlatformModelsApi';
import {
  formIsComplete,
  formOf,
  modelDraftOf,
  NO_CREDENTIAL_CHOSEN,
  TIERED_MODEL_TYPE,
  type ModelForm,
} from './platformModelForm';

/**
 * What the provider select says when nothing is chosen.
 *
 * Two different dead ends, and they need different words. With providers to
 * pick from, the operator has to pick one. With none published, no choice on
 * this screen can complete the form, and saying "select a provider" would send
 * them looking for a control that is not there.
 */
function credentialHelp(credentialNames: readonly string[], chosen: string): string {
  if (chosen !== NO_CREDENTIAL_CHOSEN) {
    return t(
      'pages.admin.platformModels.field.credentialHelp',
      'The platform provider this model authenticates through. A platform model may only use a platform provider.',
    );
  }
  if (credentialNames.length === 0) {
    return t(
      'pages.admin.platformModels.field.credentialNonePublished',
      'This platform publishes no providers yet. Add one above before publishing a model — a model with no provider is stored and never served.',
    );
  }
  return t(
    'pages.admin.platformModels.field.credentialRequired',
    'Required. A model with no provider fails admission, so it would be listed here and served to nobody.',
  );
}

export interface PlatformModelDialogProps {
  readonly open: boolean;
  readonly editing: PlatformModel | undefined;
  /** The model types this deployment dispatches, from the server. */
  readonly modelTypes: readonly string[];
  /** The platform credentials a model may name, from the server. */
  readonly credentialNames: readonly string[];
  readonly isSaving: boolean;
  readonly serverError: string | undefined;
  readonly onClose: () => void;
  readonly onSubmit: (draft: PlatformModelDraft) => void;
}

export function PlatformModelDialog({
  open,
  editing,
  modelTypes,
  credentialNames,
  isSaving,
  serverError,
  onClose,
  onSubmit,
}: PlatformModelDialogProps): ReactNode {
  const [form, setForm] = useState<ModelForm>(() => formOf(undefined, []));

  function update<K extends keyof ModelForm>(key: K, value: ModelForm[K]): void {
    setForm((current) => ({ ...current, [key]: value }));
  }

  // Reset on OPEN only: a dialog that reset while open would discard what the
  // operator was typing on every list refetch.
  useEffect(() => {
    if (!open) return;
    setForm(formOf(editing, modelTypes));
    // `modelTypes` is read rather than depended on, so a refetch cannot reset a
    // form mid-edit.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, editing]);

  const tiered = form.type === TIERED_MODEL_TYPE;
  const credentialMissing = form.credential === NO_CREDENTIAL_CHOSEN;
  const canSubmit = formIsComplete(form) && !isSaving;

  return (
    <Dialog open={open} onClose={onClose} fullWidth maxWidth="sm">
      <DialogTitle>
        {editing !== undefined
          ? t('pages.admin.platformModels.dialog.edit', 'Edit platform model')
          : t('pages.admin.platformModels.dialog.create', 'Add a platform model')}
      </DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: '1rem', paddingTop: '0.5rem' }}>
        {serverError !== undefined ? (
          <Alert severity="error" data-testid="platform-model-dialog-error">
            {serverError}
          </Alert>
        ) : null}

        <Typography variant="bodySmall" color="text.secondary">
          {t(
            'pages.admin.platformModels.dialog.intro',
            'A platform model is offered to every project on this deployment, and names the platform provider it uses. Both halves are required: a model with no provider is stored and never served.',
          )}
        </Typography>

        <TextField
          label={t('pages.admin.platformModels.field.name', 'Model ID')}
          value={form.name}
          onChange={(event) => update('name', event.target.value)}
          size="small"
          required
          slotProps={{ htmlInput: { 'data-testid': 'platform-model-name' } }}
          helperText={t(
            'pages.admin.platformModels.field.nameHelp',
            'What callers address. This is the name that appears in the model picker.',
          )}
        />

        <TextField
          label={t('pages.admin.platformModels.field.modelName', 'Provider model name')}
          value={form.modelName}
          onChange={(event) => update('modelName', event.target.value)}
          size="small"
          required
          placeholder={t('pages.admin.platformModels.field.modelNamePlaceholder', 'gpt-4o')}
          slotProps={{ htmlInput: { 'data-testid': 'platform-model-wire-name' } }}
          helperText={t(
            'pages.admin.platformModels.field.modelNameHelp',
            "The provider's own model string, sent upstream.",
          )}
        />

        <TextField
          select
          label={t('pages.admin.platformModels.field.type', 'Kind')}
          value={form.type}
          disabled={editing !== undefined}
          onChange={(event) => update('type', event.target.value)}
          size="small"
          slotProps={{ htmlInput: { 'data-testid': 'platform-model-type' } }}
          helperText={
            editing !== undefined
              ? t(
                  'pages.admin.platformModels.field.typeLocked',
                  'A model’s kind decides which endpoints serve it. To change it, delete this one and add another.',
                )
              : undefined
          }
        >
          {modelTypes.map((value) => (
            <MenuItem key={value} value={value}>
              {platformModelTypeLabel(value)}
            </MenuItem>
          ))}
        </TextField>

        <TextField
          select
          required
          error={credentialMissing}
          label={t('pages.admin.platformModels.field.credential', 'Platform provider')}
          value={form.credential}
          onChange={(event) => update('credential', event.target.value)}
          size="small"
          slotProps={{ htmlInput: { 'data-testid': 'platform-model-credential' } }}
          helperText={credentialHelp(credentialNames, form.credential)}
        >
          {credentialNames.map((value) => (
            <MenuItem key={value} value={value}>
              {value}
            </MenuItem>
          ))}
        </TextField>

        {tiered ? <PlatformModelChatFields form={form} onChange={update} /> : null}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose} disabled={isSaving}>
          {t('pages.admin.platformModels.dialog.cancel', 'Cancel')}
        </Button>
        <Button
          variant="contained"
          disabled={!canSubmit}
          data-testid="platform-model-save"
          onClick={() => {
            // The stored object is the merge base — see platformModelForm.ts.
            onSubmit(modelDraftOf(form, editing?.data));
          }}
        >
          {isSaving
            ? t('pages.admin.platformModels.dialog.saving', 'Saving…')
            : t('pages.admin.platformModels.dialog.save', 'Save')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
