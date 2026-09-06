/**
 * Create / edit one platform-wide model.
 *
 * ## The credential is a select, not a text field
 *
 * A model names its credential by TITLE, and the server refuses a title that is
 * not among the platform's published providers. Offering free text would make
 * that refusal the operator's first feedback on a name they typed correctly but
 * spelled differently — and the failure it guards against is a model advertised
 * to every project whose provider is guessed from a prefix in its name.
 *
 * "None" is a real option, not a placeholder: a model with no link resolves its
 * provider from that prefix, which is a supported configuration the standalone
 * seed relies on.
 *
 * ## The type cannot change on an edit
 *
 * The server derives the row's `section` from its type, and the gateway matches
 * on the (section, type) pair. Retyping a stored model would need both columns
 * rewritten together; the partial update writes what it is sent, so the honest
 * path is to delete and re-create.
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

import {
  platformModelTypeLabel,
  type PlatformModel,
  type PlatformModelDraft,
} from './api/adminLlmPlatformModelsApi';

/** The sentinel for "this model names no credential". */
const NO_CREDENTIAL = '';

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
  const [name, setName] = useState('');
  const [type, setType] = useState(DEFAULT_MODEL_TYPE);
  const [modelName, setModelName] = useState('');
  const [credential, setCredential] = useState<string>(NO_CREDENTIAL);

  // Reset on OPEN only: a dialog that reset while open would discard what the
  // operator was typing on every list refetch.
  useEffect(() => {
    if (!open) return;
    setName(editing?.elitea_title ?? '');
    setType(editing?.type ?? defaultModelType(modelTypes));
    setModelName(editing?.model_name ?? '');
    setCredential(editing?.credential_name ?? NO_CREDENTIAL);
    // `modelTypes` is read rather than depended on, so a refetch cannot reset a
    // form mid-edit.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, editing]);

  const canSubmit = name.trim() !== '' && modelName.trim() !== '' && !isSaving;

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
            'A platform model is offered to every project on this deployment, and uses a platform provider.',
          )}
        </Typography>

        <TextField
          label={t('pages.admin.platformModels.field.name', 'Model ID')}
          value={name}
          onChange={(event) => setName(event.target.value)}
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
          value={modelName}
          onChange={(event) => setModelName(event.target.value)}
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
          value={type}
          disabled={editing !== undefined}
          onChange={(event) => setType(event.target.value)}
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
          label={t('pages.admin.platformModels.field.credential', 'Platform provider')}
          value={credential}
          onChange={(event) => setCredential(event.target.value)}
          size="small"
          slotProps={{ htmlInput: { 'data-testid': 'platform-model-credential' } }}
          helperText={t(
            'pages.admin.platformModels.field.credentialHelp',
            'Without one, the provider is guessed from the model name.',
          )}
        >
          <MenuItem value={NO_CREDENTIAL}>
            {t('pages.admin.platformModels.field.noCredential', 'None — infer from the model name')}
          </MenuItem>
          {credentialNames.map((value) => (
            <MenuItem key={value} value={value}>
              {value}
            </MenuItem>
          ))}
        </TextField>
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
            onSubmit({
              elitea_title: name.trim(),
              type,
              data: {
                name: modelName.trim(),
                // The link is OMITTED when there is none, rather than sent as an
                // empty object: the gateway treats a link naming nothing as no
                // link, but writing one would make a row that says it has a
                // credential and does not.
                ...(credential !== NO_CREDENTIAL
                  ? { ai_credentials: { elitea_title: credential } }
                  : {}),
              },
            });
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
