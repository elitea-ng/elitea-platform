/**
 * Register or edit one Microsoft app (Admin › Configuration › Microsoft apps).
 *
 * The rules are the SERVER's: the dialog shows what the server refused beside the
 * field it names (`field` in the 400), and stays open holding what was typed. The
 * client secret is write-only: the field starts empty on an edit, an empty field sends
 * nothing (the sealed secret is kept), and the secret is dropped from the form the
 * moment the dialog closes.
 */
import { useEffect, useState } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Divider from '@mui/material/Divider';
import TextField from '@mui/material/TextField';

import type { MicrosoftApp, MicrosoftAppRequest, MicrosoftAppTypePermissions } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import FormControlLabel from '@mui/material/FormControlLabel';

import { AppChecklist } from './AdminMicrosoftAppChecklist';
import {
  CeilingFields,
  ClientChangeWarning,
  CredentialTypesField,
  ProjectScopeField,
  SecretFields,
  type MicrosoftAppFormUpdate,
} from './AdminMicrosoftAppFields';
import {
  emptyMicrosoftAppForm,
  isFormRefusal,
  microsoftAppFormOf,
  microsoftAppRequest,
  type MicrosoftAppForm,
} from './adminMicrosoftAppForm';

export interface AdminMicrosoftAppDialogProps {
  readonly open: boolean;
  /** `undefined` registers a new app; an app edits it. */
  readonly editing: MicrosoftApp | undefined;
  readonly typeInfo: readonly MicrosoftAppTypePermissions[];
  readonly isSaving: boolean;
  /** A refusal that names no field, in the server's words. */
  readonly serverError: string | undefined;
  /** The field the last refusal named, with the server's sentence. */
  readonly fieldError: { readonly field: string; readonly message: string } | undefined;
  readonly onClose: () => void;
  readonly onSubmit: (body: MicrosoftAppRequest) => void;
}

function errorFor(fieldError: AdminMicrosoftAppDialogProps['fieldError'], field: string): string | undefined {
  return fieldError?.field === field ? fieldError.message : undefined;
}

interface TextFieldSpec {
  readonly key: 'name' | 'description' | 'clientId' | 'authority';
  readonly serverField: string;
  readonly label: string;
  readonly help: string | undefined;
  readonly testId: string;
  readonly required: boolean;
}

function textFieldSpecs(): readonly TextFieldSpec[] {
  return [
    {
      key: 'name',
      serverField: 'name',
      label: t('pages.admin.microsoftApps.dialog.name', 'Name'),
      help: t('pages.admin.microsoftApps.dialog.nameHelp', 'Shown to people in the credential form.'),
      testId: 'microsoft-app-name',
      required: true,
    },
    {
      key: 'description',
      serverField: 'description',
      label: t('pages.admin.microsoftApps.dialog.description', 'Description'),
      help: undefined,
      testId: 'microsoft-app-description',
      required: false,
    },
    {
      key: 'clientId',
      serverField: 'client_id',
      label: t('pages.admin.microsoftApps.dialog.clientId', 'Application (client) ID'),
      help: t('pages.admin.microsoftApps.dialog.clientIdHelp', 'From the Entra app registration, Overview.'),
      testId: 'microsoft-app-client-id',
      required: true,
    },
    {
      key: 'authority',
      serverField: 'authority',
      label: t('pages.admin.microsoftApps.dialog.authority', 'Authority'),
      help: t(
        'pages.admin.microsoftApps.dialog.authorityHelp',
        'https://login.microsoftonline.com/ followed by the tenant ID, a verified domain, organizations or common.',
      ),
      testId: 'microsoft-app-authority',
      required: true,
    },
  ];
}

function TextFields({
  form,
  update,
  fieldError,
}: {
  readonly form: MicrosoftAppForm;
  readonly update: MicrosoftAppFormUpdate;
  readonly fieldError: AdminMicrosoftAppDialogProps['fieldError'];
}) {
  return (
    <>
      {textFieldSpecs().map((spec) => (
        <TextField
          key={spec.key}
          label={spec.label}
          value={form[spec.key]}
          onChange={(event) => {
            update(spec.key, event.target.value);
          }}
          fullWidth
          required={spec.required}
          error={errorFor(fieldError, spec.serverField) !== undefined}
          helperText={errorFor(fieldError, spec.serverField) ?? spec.help}
          slotProps={{ htmlInput: { 'data-testid': spec.testId } }}
        />
      ))}
    </>
  );
}

export function AdminMicrosoftAppDialog({
  open,
  editing,
  typeInfo,
  isSaving,
  serverError,
  fieldError,
  onClose,
  onSubmit,
}: AdminMicrosoftAppDialogProps) {
  const isEdit = editing !== undefined;
  const [form, setForm] = useState<MicrosoftAppForm>(() => emptyMicrosoftAppForm(typeInfo));
  const [localError, setLocalError] = useState<string | undefined>(undefined);

  useEffect(() => {
    if (open) {
      setForm(editing === undefined ? emptyMicrosoftAppForm(typeInfo) : microsoftAppFormOf(editing, typeInfo));
      setLocalError(undefined);
    } else {
      // The secret never outlives the dialog.
      setForm((previous) => ({ ...previous, secret: '' }));
    }
  }, [open, editing, typeInfo]);

  const update: MicrosoftAppFormUpdate = (key, value) => {
    setForm((previous) => ({ ...previous, [key]: value }));
  };

  const clientChanged = isEdit && editing.secret_set && form.clientId.trim().toLowerCase() !== editing.client_id;

  const submit = () => {
    const request = microsoftAppRequest(form);
    if (isFormRefusal(request)) {
      setLocalError(t('pages.admin.microsoftApps.dialog.projectIdsInvalid', 'Enter numeric project IDs separated by commas.'));
      return;
    }
    setLocalError(undefined);
    onSubmit(request);
  };

  return (
    <Dialog open={open} onClose={onClose} maxWidth="md" fullWidth data-testid="microsoft-app-dialog">
      <DialogTitle>
        {isEdit
          ? t('pages.admin.microsoftApps.dialog.editTitle', 'Edit Microsoft app')
          : t('pages.admin.microsoftApps.dialog.createTitle', 'Register Microsoft app')}
      </DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: '1rem', '&&': { pt: '0.75rem' } }}>
        {serverError === undefined ? null : (
          <Alert severity="error" data-testid="microsoft-app-dialog-error">
            {serverError}
          </Alert>
        )}
        <TextFields form={form} update={update} fieldError={fieldError} />
        <CredentialTypesField form={form} update={update} error={errorFor(fieldError, 'credential_types')} />
        <CeilingFields form={form} update={update} typeInfo={typeInfo} />
        <FormControlLabel
          label={t('pages.admin.microsoftApps.dialog.autoRefresh', 'Ask for offline_access, so a sign-in can be refreshed')}
          control={
            <BaseCheckbox
              checked={form.autoRefresh}
              onChange={(event) => {
                update('autoRefresh', event.target.checked);
              }}
            />
          }
        />
        <ProjectScopeField form={form} update={update} error={localError ?? errorFor(fieldError, 'project_ids')} />
        <ClientChangeWarning show={clientChanged} />
        <SecretFields
          form={form}
          update={update}
          secretSet={isEdit && editing.secret_set}
          error={errorFor(fieldError, 'secret')}
        />
        {isEdit ? (
          <>
            <Divider />
            <AppChecklist checklist={editing.checklist} />
          </>
        ) : null}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose} disabled={isSaving} sx={{ textTransform: 'none' }}>
          {t('pages.admin.microsoftApps.dialog.cancel', 'Cancel')}
        </Button>
        <Button
          variant="elitea"
          color="primary"
          onClick={submit}
          disabled={isSaving}
          sx={{ textTransform: 'none' }}
          data-testid="microsoft-app-save"
        >
          {t('pages.admin.microsoftApps.dialog.save', 'Save')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
