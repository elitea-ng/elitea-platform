/**
 * Create / edit one platform-wide model.
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
 * ## The tier flags are only offered where the schema has them
 *
 * `low_tier` and `high_tier` are `llm_model` fields. The other four model types
 * decode their `data` with unknown fields REFUSED, so sending a flag they do not
 * declare would turn a valid model into an invalid binding.
 */
import type { ReactNode } from 'react';
import { useEffect, useState } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Checkbox from '@mui/material/Checkbox';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import FormControlLabel from '@mui/material/FormControlLabel';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import {
  platformModelTypeLabel,
  type PlatformModel,
  type PlatformModelDraft,
} from './api/adminLlmPlatformModelsApi';

/** The value of the provider select before one has been chosen. */
const NO_CREDENTIAL_CHOSEN = '';

/** The one model type whose schema declares the tier flags. */
const TIERED_MODEL_TYPE = 'llm_model';

/**
 * The Kind a NEW platform model opens on.
 *
 * The dialog took `modelTypes[0]`, and `model_types` arrives in the server's
 * own order, not in an order this screen chose. On this deployment the first
 * entry is `asr_model`, so "Add a platform model" opened on "Speech to text" —
 * an operator adding a chat model had to notice the wrong Kind and change it,
 * and one who did not published a model the gateway dispatches to the ASR
 * section. Chat is what almost every platform model is.
 *
 * The server still decides what is OFFERED: this default is used only when the
 * deployment actually dispatches it, and a deployment that does not falls back
 * to the first type it does.
 */
const DEFAULT_MODEL_TYPE = 'llm_model';

/** The Kind to open on: chat when this deployment dispatches it, else whatever it does. */
function defaultModelType(modelTypes: readonly string[]): string {
  if (modelTypes.includes(DEFAULT_MODEL_TYPE)) return DEFAULT_MODEL_TYPE;
  return modelTypes[0] ?? DEFAULT_MODEL_TYPE;
}

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

/** The tier flags, offered for the one type whose schema declares them. */
function TierFlags({
  low,
  high,
  onLow,
  onHigh,
}: {
  readonly low: boolean;
  readonly high: boolean;
  readonly onLow: (value: boolean) => void;
  readonly onHigh: (value: boolean) => void;
}): ReactNode {
  return (
    <>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.platformModels.field.tiersHelp',
          'A tier decides which of a project’s default model slots may select this model. A model in no tier is still addressable by name.',
        )}
      </Typography>
      <FormControlLabel
        control={
          <Checkbox
            data-testid="platform-model-low-tier"
            checked={low}
            onChange={(event) => {
              onLow(event.target.checked);
            }}
          />
        }
        label={t('pages.admin.platformModels.field.lowTier', 'Offer as a low-tier default')}
      />
      <FormControlLabel
        control={
          <Checkbox
            data-testid="platform-model-high-tier"
            checked={high}
            onChange={(event) => {
              onHigh(event.target.checked);
            }}
          />
        }
        label={t('pages.admin.platformModels.field.highTier', 'Offer as a high-tier default')}
      />
    </>
  );
}

/** Everything the form holds, so one reset statement can restate all of it. */
interface ModelForm {
  readonly name: string;
  readonly type: string;
  readonly modelName: string;
  readonly credential: string;
  readonly lowTier: boolean;
  readonly highTier: boolean;
}

/**
 * The form a dialog OPENS on: the stored row's own values, or the blanks a new
 * model starts from.
 *
 * The tier flags are read back from the row rather than defaulted, for the
 * reason the credential is: the update replaces `data` whole, so a form that
 * opened with both boxes clear would clear the tier of every model it saved.
 */
function formOf(editing: PlatformModel | undefined, modelTypes: readonly string[]): ModelForm {
  // Split rather than six optional chains, so the create case reads as the
  // blanks it is — the same split `normalisePlatformModels` makes.
  if (editing === undefined) {
    return {
      name: '',
      type: defaultModelType(modelTypes),
      modelName: '',
      credential: NO_CREDENTIAL_CHOSEN,
      lowTier: false,
      highTier: false,
    };
  }
  // `credential_name` is empty on a row written before the link was required.
  // It opens the form with the provider unchosen and Save disabled, which is
  // the state that row is in.
  return {
    name: editing.elitea_title,
    type: editing.type,
    modelName: editing.model_name,
    credential: editing.credential_name,
    lowTier: editing.low_tier ?? false,
    highTier: editing.high_tier ?? false,
  };
}

/**
 * Whether Save may act. The provider is one of the three, and it is the one
 * that used to have a value meaning "none".
 */
function formIsComplete(form: ModelForm): boolean {
  return (
    form.name.trim() !== '' &&
    form.modelName.trim() !== '' &&
    form.credential !== NO_CREDENTIAL_CHOSEN
  );
}

/**
 * The body the form sends.
 *
 * The tier flags go out for the chat type ALONE. The other four decode their
 * `data` with unknown fields refused, so a flag they do not declare would make
 * the row an invalid binding rather than a model with an ignored extra.
 */
function modelDraftOf(form: ModelForm): PlatformModelDraft {
  const tiers = form.type === TIERED_MODEL_TYPE
    ? { low_tier: form.lowTier, high_tier: form.highTier }
    : {};
  return {
    elitea_title: form.name.trim(),
    type: form.type,
    data: {
      name: form.modelName.trim(),
      ai_credentials: { elitea_title: form.credential },
      ...tiers,
    },
  };
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

        {tiered ? (
          <TierFlags
            low={form.lowTier}
            high={form.highTier}
            onLow={(value) => update('lowTier', value)}
            onHigh={(value) => update('highTier', value)}
          />
        ) : null}
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
            onSubmit(modelDraftOf(form));
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
