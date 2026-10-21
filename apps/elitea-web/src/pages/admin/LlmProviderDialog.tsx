/**
 * Create / edit one platform-wide LLM provider credential.
 *
 * One component for both, as `./AdminMcpServerDialog.tsx` is, and for the same
 * reason: the two differ only in whether the type may change and whether a
 * secret is required, and splitting them would duplicate every field.
 *
 * ## A secret is never pre-filled, because it is never sent
 *
 * The list route returns whether each secret is SET and whether it is SEALED,
 * never its value — so there is nothing to pre-fill with, and fetching the
 * plaintext in order to display it would put a provider key on screen because a
 * dialog opened.
 *
 * That makes "unchanged" the default on an edit: an untouched secret field sends
 * no key at all and the sealed one stays. The helper text says so, because an
 * empty field that means "keep the current value" is otherwise indistinguishable
 * from one that means "there is none".
 *
 * ## The type cannot change on an edit
 *
 * A provider's fields ARE its type. Retyping a stored credential would leave the
 * previous provider's fields in the row — a Bedrock credential keeping its AWS
 * keys after becoming an OpenAI one — and the partial update would not remove
 * them. Deleting and re-creating is the honest path, and it is one action more.
 */
import type { ReactNode } from 'react';
import { useEffect, useState } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import FormControlLabel from '@mui/material/FormControlLabel';
import FormHelperText from '@mui/material/FormHelperText';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import { LlmProviderAuthModeField } from './LlmProviderAuthModeField';
import {
  endpointOrigin,
  missingProviderFields,
  providerAuthMode,
  providerDataFor,
  providerFields,
  providerTypeLabel,
  supportsWorkloadIdentity,
  type KeylessProviderListing,
  type ProviderField,
} from './llmProviderForm';
import {
  LLM_PROVIDER_TYPES,
  type LlmProvider,
  type LlmProviderDraft,
  type LlmProviderType,
} from './api/adminLlmProvidersApi';

export interface LlmProviderDialogProps {
  readonly open: boolean;
  /**
   * The types THIS deployment will publish, from the server. Narrower than
   * `LLM_PROVIDER_TYPES`, which is only what this app can draw a form for.
   */
  readonly providerTypes: readonly string[];
  /** `undefined` ⇒ create; a row ⇒ edit it. */
  readonly editing: LlmProvider | undefined;
  readonly isSaving: boolean;
  /** The server's own words when the last attempt was refused. */
  readonly serverError: string | undefined;
  readonly onClose: () => void;
  readonly onSubmit: (draft: LlmProviderDraft) => void;
}

/**
 * The non-secret values an edit can pre-fill, from what the listing published.
 *
 * `auth_mode` and `token_audience` are read from `settings` or from the row
 * itself, whichever the server sends them in. Absent is the key mode.
 */
function initialValuesFor(
  editing: (LlmProvider & KeylessProviderListing) | undefined,
): Record<string, string> {
  if (editing === undefined) return {};
  const values: Record<string, string> = { api_base: editing.endpoint, ...editing.settings };
  if (editing.auth_mode !== undefined) values['auth_mode'] = editing.auth_mode;
  if (editing.token_audience !== undefined) values['token_audience'] = editing.token_audience;
  return values;
}

/** A field's placeholder. The token audience defaults to the endpoint origin. */
function placeholderFor(
  field: ProviderField,
  values: Readonly<Record<string, string>>,
): string | undefined {
  if (field.key !== 'token_audience') return field.placeholder;
  const origin = endpointOrigin(values['api_base'] ?? '');
  return origin === '' ? undefined : origin;
}

export function LlmProviderDialog({
  open,
  providerTypes,
  editing,
  isSaving,
  serverError,
  onClose,
  onSubmit,
}: LlmProviderDialogProps): ReactNode {
  const [name, setName] = useState('');
  const [type, setType] = useState<LlmProviderType>('open_ai');
  const [values, setValues] = useState<Record<string, string>>({});

  // The intersection: what the server admits AND this app can draw. A type the
  // server admits and this build has no field definitions for is DROPPED rather
  // than offered as an empty form — the operator would save a credential with no
  // key in it and be told it succeeded.
  const offered = LLM_PROVIDER_TYPES.filter((known) => providerTypes.includes(known));

  // Reset on OPEN, not on every render of a closed dialog: a dialog that reset
  // while open would discard what the operator was typing whenever the parent
  // re-rendered — which it does on every list refetch.
  useEffect(() => {
    if (!open) return;
    setName(editing?.elitea_title ?? '');
    setType((editing?.type as LlmProviderType | undefined) ?? offered[0] ?? 'open_ai');
    setValues(initialValuesFor(editing));
    // `offered` is derived from props and is stable for one open dialog; it is
    // read rather than depended on so a list refetch cannot reset a form the
    // operator is filling in.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, editing]);

  const authMode = providerAuthMode(values);
  const fields = providerFields(type, authMode);
  const missing = missingProviderFields(type, values, editing !== undefined);
  const canSubmit = name.trim() !== '' && missing.length === 0 && !isSaving;

  return (
    <Dialog open={open} onClose={onClose} fullWidth maxWidth="sm">
      <DialogTitle>
        {editing !== undefined
          ? t('pages.admin.llmProviders.dialog.edit', 'Edit platform provider')
          : t('pages.admin.llmProviders.dialog.create', 'Add a platform provider')}
      </DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: '1rem', paddingTop: '0.5rem' }}>
        {/* The server's sentence, not a generic failure. Its refusals here are
            specific — a self-referential endpoint, a type the gateway cannot
            dispatch to — and those words are the only ones that say what to
            change. */}
        {serverError !== undefined ? (
          <Alert severity="error" data-testid="llm-provider-dialog-error">
            {serverError}
          </Alert>
        ) : null}

        <Typography variant="bodySmall" color="text.secondary">
          {t(
            'pages.admin.llmProviders.dialog.intro',
            'A platform provider is available to every project on this deployment. The key is sealed in the platform vault and is never shown again.',
          )}
        </Typography>

        <TextField
          label={t('pages.admin.llmProviders.field.name', 'Name')}
          value={name}
          onChange={(event) => setName(event.target.value)}
          size="small"
          required
          slotProps={{ htmlInput: { 'data-testid': 'llm-provider-name' } }}
          helperText={t(
            'pages.admin.llmProviders.field.nameHelp',
            'Shown to every project. A model binds to a credential by this name.',
          )}
        />

        <TextField
          select
          label={t('pages.admin.llmProviders.field.type', 'Provider')}
          value={type}
          // A stored credential's type is fixed — see the header.
          disabled={editing !== undefined}
          onChange={(event) => {
            setType(event.target.value as LlmProviderType);
            // The previous type's values are DISCARDED rather than carried
            // over. They are a different provider's fields, and keeping them
            // would send an Azure api-version on an OpenAI credential.
            setValues({});
          }}
          size="small"
          slotProps={{ htmlInput: { 'data-testid': 'llm-provider-type' } }}
          helperText={
            editing !== undefined
              ? t(
                  'pages.admin.llmProviders.field.typeLocked',
                  "A credential's fields are its type. To change it, delete this one and add another.",
                )
              : undefined
          }
        >
          {offered.map((value) => (
            <MenuItem key={value} value={value}>
              {providerTypeLabel(value)}
            </MenuItem>
          ))}
        </TextField>

        {supportsWorkloadIdentity(type) ? (
          <LlmProviderAuthModeField
            value={authMode}
            onChange={(mode) => {
              setValues((current) => ({ ...current, auth_mode: mode }));
            }}
          />
        ) : null}

        {fields.map((field) =>
          field.kind === 'boolean' ? (
            <div key={field.key}>
              <FormControlLabel
                control={
                  <BaseCheckbox
                    checked={values[field.key] === 'true'}
                    onChange={(event) => {
                      setValues((current) => ({
                        ...current,
                        [field.key]: event.target.checked ? 'true' : 'false',
                      }));
                    }}
                    data-testid={`llm-provider-${field.key}`}
                  />
                }
                label={field.label}
              />
              {field.helperText !== undefined ? (
                <FormHelperText>{field.helperText}</FormHelperText>
              ) : null}
            </div>
          ) : (
            <TextField
              key={field.key}
              label={field.label}
              value={values[field.key] ?? ''}
              onChange={(event) => {
                setValues((current) => ({ ...current, [field.key]: event.target.value }));
              }}
              size="small"
              type={field.secret && field.multiline !== true ? 'password' : 'text'}
              multiline={field.multiline === true}
              minRows={field.multiline === true ? 4 : undefined}
              required={field.required && !(field.secret && editing !== undefined)}
              placeholder={placeholderFor(field, values)}
              slotProps={{ htmlInput: { 'data-testid': `llm-provider-${field.key}` } }}
              helperText={
                field.secret && editing !== undefined
                  ? t(
                      'pages.admin.llmProviders.field.secretUnchanged',
                      'Leave blank to keep the stored value. It cannot be read back.',
                    )
                  : field.helperText
              }
            />
          ),
        )}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose} disabled={isSaving}>
          {t('pages.admin.llmProviders.dialog.cancel', 'Cancel')}
        </Button>
        <Button
          variant="contained"
          disabled={!canSubmit}
          data-testid="llm-provider-save"
          onClick={() => {
            onSubmit({
              elitea_title: name.trim(),
              type,
              data: providerDataFor(type, values),
            });
          }}
        >
          {isSaving
            ? t('pages.admin.llmProviders.dialog.saving', 'Saving…')
            : t('pages.admin.llmProviders.dialog.save', 'Save')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
