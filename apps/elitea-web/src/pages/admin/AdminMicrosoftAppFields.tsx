/**
 * The field groups of the Microsoft app dialog: the credential types, the permission
 * ceiling of each, the project scope and the write-only secret. Split from
 * `AdminMicrosoftAppDialog` so each group stays a small component.
 */
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import FormControl from '@mui/material/FormControl';
import FormControlLabel from '@mui/material/FormControlLabel';
import FormGroup from '@mui/material/FormGroup';
import FormHelperText from '@mui/material/FormHelperText';
import FormLabel from '@mui/material/FormLabel';
import Radio from '@mui/material/Radio';
import RadioGroup from '@mui/material/RadioGroup';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { MicrosoftAppTypePermissions } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import {
  hasCeiling,
  isFreeTextType,
  MICROSOFT_APP_TYPES,
  toggleName,
  type MicrosoftAppForm,
  type MicrosoftAppProjectScope,
} from './adminMicrosoftAppForm';

export type MicrosoftAppFormUpdate = <K extends keyof MicrosoftAppForm>(key: K, value: MicrosoftAppForm[K]) => void;

const TYPE_LABELS: Readonly<Record<string, string>> = {
  outlook: 'Outlook',
  teams: 'Teams',
  ms_graph: 'Microsoft Graph',
  dataverse: 'Dataverse',
  sharepoint: 'SharePoint',
};

export function typeLabel(type: string): string {
  return TYPE_LABELS[type] ?? type;
}

export function CredentialTypesField({
  form,
  update,
  error,
}: {
  readonly form: MicrosoftAppForm;
  readonly update: MicrosoftAppFormUpdate;
  readonly error: string | undefined;
}) {
  return (
    <FormControl error={error !== undefined} data-testid="microsoft-app-types">
      <FormLabel>{t('pages.admin.microsoftApps.dialog.types', 'Credential types this app serves')}</FormLabel>
      <FormGroup row>
        {MICROSOFT_APP_TYPES.map((type) => (
          <FormControlLabel
            key={type}
            label={typeLabel(type)}
            control={
              <BaseCheckbox
                checked={form.types.includes(type)}
                onChange={(event) => {
                  update(
                    'types',
                    event.target.checked ? [...form.types, type] : form.types.filter((current) => current !== type),
                  );
                }}
              />
            }
          />
        ))}
      </FormGroup>
      {error === undefined ? null : <FormHelperText>{error}</FormHelperText>}
    </FormControl>
  );
}

function FixedCeiling({
  type,
  info,
  form,
  update,
}: {
  readonly type: string;
  readonly info: MicrosoftAppTypePermissions;
  readonly form: MicrosoftAppForm;
  readonly update: MicrosoftAppFormUpdate;
}) {
  const offered = info.offered.map((permission) => permission.name);
  const current = form.ticked[type] ?? [];
  return (
    <FormGroup row>
      {info.offered.map((permission) => (
        <FormControlLabel
          key={permission.name}
          label={
            permission.admin_consent
              ? t('pages.admin.microsoftApps.checklist.needsConsent', '{{name}} (admin consent)', { name: permission.name })
              : permission.name
          }
          control={
            <BaseCheckbox
              checked={current.includes(permission.name)}
              onChange={() => {
                update('ticked', { ...form.ticked, [type]: toggleName(current, offered, permission.name) });
              }}
            />
          }
        />
      ))}
    </FormGroup>
  );
}

function CeilingField({
  type,
  info,
  form,
  update,
}: {
  readonly type: string;
  readonly info: MicrosoftAppTypePermissions;
  readonly form: MicrosoftAppForm;
  readonly update: MicrosoftAppFormUpdate;
}) {
  return (
    <Box data-testid={`microsoft-app-ceiling-${type}`}>
      <Typography variant="bodySmall" color="text.secondary">
        {t('pages.admin.microsoftApps.dialog.ceiling', '{{type}}: permissions this app may ask for', { type: typeLabel(type) })}
      </Typography>
      {isFreeTextType(type) ? (
        <TextField
          value={form.typed[type] ?? ''}
          onChange={(event) => {
            update('typed', { ...form.typed, [type]: event.target.value });
          }}
          size="small"
          fullWidth
          placeholder={info.default.join(' ')}
          helperText={t(
            'pages.admin.microsoftApps.dialog.ceilingFreeText',
            'Microsoft Graph permission names separated by spaces, for example Files.Read Sites.Read.All. Empty means the default shown.',
          )}
          slotProps={{ htmlInput: { 'aria-label': typeLabel(type), 'data-testid': `microsoft-app-ceiling-text-${type}` } }}
        />
      ) : (
        <FixedCeiling type={type} info={info} form={form} update={update} />
      )}
    </Box>
  );
}

export function CeilingFields({
  form,
  update,
  typeInfo,
}: {
  readonly form: MicrosoftAppForm;
  readonly update: MicrosoftAppFormUpdate;
  readonly typeInfo: readonly MicrosoftAppTypePermissions[];
}) {
  const rows = form.types.filter(hasCeiling);
  return (
    <>
      {rows.map((type) => {
        const info = typeInfo.find((candidate) => candidate.type === type);
        return info === undefined ? null : <CeilingField key={type} type={type} info={info} form={form} update={update} />;
      })}
      {form.types.some((type) => !hasCeiling(type)) ? (
        <Typography variant="bodySmall" color="text.secondary">
          {t(
            'pages.admin.microsoftApps.dialog.dataverseNote',
            'Dataverse has no permission list: its one scope is user_impersonation on the environment named in each credential.',
          )}
        </Typography>
      ) : null}
    </>
  );
}

export function ProjectScopeField({
  form,
  update,
  error,
}: {
  readonly form: MicrosoftAppForm;
  readonly update: MicrosoftAppFormUpdate;
  readonly error: string | undefined;
}) {
  return (
    <FormControl error={error !== undefined}>
      <FormLabel>{t('pages.admin.microsoftApps.dialog.projects', 'Projects that may use this app')}</FormLabel>
      <RadioGroup
        row
        value={form.projectScope}
        onChange={(event) => {
          update('projectScope', event.target.value as MicrosoftAppProjectScope);
        }}
      >
        <FormControlLabel value="all" control={<Radio />} label={t('pages.admin.microsoftApps.dialog.projectsAll', 'Every project')} />
        <FormControlLabel
          value="selected"
          control={<Radio />}
          label={t('pages.admin.microsoftApps.dialog.projectsSelected', 'Selected projects')}
        />
      </RadioGroup>
      {form.projectScope === 'selected' ? (
        <TextField
          value={form.projectIds}
          onChange={(event) => {
            update('projectIds', event.target.value);
          }}
          size="small"
          label={t('pages.admin.microsoftApps.dialog.projectIds', 'Project IDs')}
          helperText={error ?? t('pages.admin.microsoftApps.dialog.projectIdsHelp', 'Numeric project IDs separated by commas.')}
          error={error !== undefined}
          slotProps={{ htmlInput: { 'data-testid': 'microsoft-app-project-ids' } }}
        />
      ) : null}
    </FormControl>
  );
}

export function SecretFields({
  form,
  update,
  secretSet,
  error,
}: {
  readonly form: MicrosoftAppForm;
  readonly update: MicrosoftAppFormUpdate;
  readonly secretSet: boolean;
  readonly error: string | undefined;
}) {
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
      <TextField
        label={t('pages.admin.microsoftApps.dialog.secret', 'Client secret')}
        type="password"
        value={form.secret}
        onChange={(event) => {
          update('secret', event.target.value);
        }}
        disabled={form.clearSecret}
        fullWidth
        autoComplete="new-password"
        error={error !== undefined}
        helperText={
          error ??
          (secretSet
            ? t('pages.admin.microsoftApps.dialog.secretSet', 'A secret is stored and cannot be shown. Leave empty to keep it, or enter a new one to replace it.')
            : t('pages.admin.microsoftApps.dialog.secretNew', 'The value of the client secret from Entra. Without it the app stays a draft. It is sealed and never shown again.'))
        }
        slotProps={{ htmlInput: { 'data-testid': 'microsoft-app-secret' } }}
      />
      {secretSet ? (
        <FormControlLabel
          label={t('pages.admin.microsoftApps.dialog.clearSecret', 'Remove the stored secret (the app becomes a draft)')}
          control={
            <BaseCheckbox
              checked={form.clearSecret}
              onChange={(event) => {
                update('clearSecret', event.target.checked);
              }}
            />
          }
        />
      ) : null}
      <TextField
        label={t('pages.admin.microsoftApps.dialog.expiry', 'Secret expires on')}
        type="date"
        value={form.secretExpiresAt}
        onChange={(event) => {
          update('secretExpiresAt', event.target.value);
        }}
        slotProps={{ inputLabel: { shrink: true }, htmlInput: { 'data-testid': 'microsoft-app-expiry-date' } }}
        helperText={t('pages.admin.microsoftApps.dialog.expiryHelp', 'Optional. This page warns 30 days before.')}
      />
    </Box>
  );
}

export function ClientChangeWarning({ show }: { readonly show: boolean }) {
  if (!show) return null;
  return (
    <Alert severity="warning" data-testid="microsoft-app-client-change">
      {t(
        'pages.admin.microsoftApps.dialog.clientChanged',
        'The client ID changed. Enter the client secret of the new client; people who signed in with the old client sign in again.',
      )}
    </Alert>
  );
}
