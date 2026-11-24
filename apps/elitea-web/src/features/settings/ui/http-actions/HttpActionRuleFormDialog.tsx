/**
 * HttpActionRuleFormDialog — create or edit one project HTTP action rule
 * (T-5D), in the same `BaseModal` form pattern as `WebhookFormDialog`.
 *
 * The server owns validation (rule schema, egress guard, uniqueness, the
 * 256-rule limit); the form only refuses blank required fields so an obvious
 * omission does not cost a round trip. A server refusal is shown inline
 * above the fields until the next edit, and the dialog stays open with the
 * user's input.
 */
import { useEffect, useState } from 'react';

import Autocomplete from '@mui/material/Autocomplete';
import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { HttpActionRuleWriteRequest } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { BaseModal } from '@/shared/ui/BaseModal';
import { InputBase } from '@/shared/ui/InputBase';
import { SingleSelect } from '@/shared/ui/SingleSelect';

import { useOpenApiCredentialOptions } from '../../lib/http-actions/useOpenApiCredentialOptions';

/** The write body minus `revision`, with the two optional fields made concrete for the inputs. */
export type HttpActionRuleFormValues = Omit<HttpActionRuleWriteRequest, 'revision' | 'output_bucket' | 'replay'> & {
  output_bucket: string;
  replay: NonNullable<HttpActionRuleWriteRequest['replay']>;
};

export interface HttpActionRuleFormDialogProps {
  readonly open: boolean;
  readonly projectId: string;
  readonly isSaving: boolean;
  /** Present when editing an existing rule; absent for create. */
  readonly initialValues?: HttpActionRuleFormValues | undefined;
  /** The server's refusal of the LAST submit (400 field/egress error, 409 conflict). */
  readonly serverError?: string | undefined;
  readonly onClose: () => void;
  readonly onSubmit: (values: HttpActionRuleFormValues) => void;
}

const MAX_QUERY_NAMES = 32;
const METHODS: HttpActionRuleFormValues['method'][] = ['GET', 'HEAD', 'OPTIONS', 'POST', 'PUT', 'PATCH', 'DELETE'];
type TextKey = 'name' | 'origin' | 'path_template' | 'toolkit' | 'tool' | 'output_bucket';
const REQUIRED: TextKey[] = ['name', 'origin', 'path_template', 'toolkit', 'tool'];

const EMPTY_VALUES: HttpActionRuleFormValues = {
  name: '',
  origin: '',
  path_template: '',
  method: 'GET',
  query_names: [],
  configuration_id: 0,
  toolkit: '',
  tool: '',
  output_bucket: '',
  replay: 'never',
  enabled: true,
};

const contentSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1.25rem', minWidth: '25rem' };
const helperSx: SxProps<Theme> = { color: 'text.secondary', marginTop: '0.25rem' };

export function HttpActionRuleFormDialog({ open, projectId, isSaving, initialValues, serverError, onClose, onSubmit }: HttpActionRuleFormDialogProps) {
  const [values, setValues] = useState<HttpActionRuleFormValues>(EMPTY_VALUES);
  const [missing, setMissing] = useState<ReadonlySet<string>>(new Set());
  const [serverErrorDismissed, setServerErrorDismissed] = useState(false);
  const credentialOptions = useOpenApiCredentialOptions(projectId, open, values.configuration_id);

  // Reset to the rule being edited (or blank) every time the dialog opens.
  useEffect(() => {
    if (!open) return;
    setValues(initialValues ?? EMPTY_VALUES);
    setMissing(new Set());
    setServerErrorDismissed(false);
  }, [open, initialValues]);

  // A NEW server refusal is shown again even if the previous one was dismissed.
  useEffect(() => {
    setServerErrorDismissed(false);
  }, [serverError]);

  const update = <K extends keyof HttpActionRuleFormValues>(key: K, value: HttpActionRuleFormValues[K]) => {
    setValues((previous) => ({ ...previous, [key]: value }));
    setMissing((previous) => (previous.has(key) ? new Set([...previous].filter((entry) => entry !== key)) : previous));
    setServerErrorDismissed(true);
  };

  const handleSubmit = () => {
    const trimmed = { ...values };
    for (const key of [...REQUIRED, 'output_bucket'] as TextKey[]) trimmed[key] = values[key].trim();
    const blank = REQUIRED.filter((key) => trimmed[key] === '');
    if (blank.length > 0) {
      setMissing(new Set(blank));
      return;
    }
    onSubmit(trimmed);
  };

  const textField = (key: TextKey, label: string, placeholder: string) => (
    <InputBase
      label={label}
      placeholder={placeholder}
      value={values[key]}
      onChange={(event) => update(key, event.target.value)}
      error={missing.has(key)}
      helperText={missing.has(key) ? t('entities.httpActionRule.form.required', 'This field is required') : undefined}
      data-testid={`http-action-rule-form-${key.replace('_', '-')}`}
    />
  );

  const isEdit = initialValues !== undefined;
  const shownServerError = !serverErrorDismissed && serverError ? serverError : '';

  return (
    <BaseModal
      open={open}
      onClose={onClose}
      title={isEdit
        ? t('entities.httpActionRule.form.editTitle', 'Edit HTTP action rule')
        : t('entities.httpActionRule.form.createTitle', 'New HTTP action rule')}
      content={
        <Box sx={contentSx}>
          {shownServerError && <BannerMessage variant="error" message={shownServerError} />}
          {textField('name', t('entities.httpActionRule.form.nameLabel', 'Name *'), 'crm.lookup_customer')}
          {textField('origin', t('entities.httpActionRule.form.originLabel', 'Origin *'), 'https://api.example.com')}
          {textField('path_template', t('entities.httpActionRule.form.pathLabel', 'Path template *'), '/customers/{customer_id}')}
          <SingleSelect
            label={t('entities.httpActionRule.form.methodLabel', 'Method')}
            value={values.method}
            options={METHODS.map((method) => ({ value: method, label: method }))}
            onChange={(next) => update('method', next as HttpActionRuleFormValues['method'])}
          />
          <Autocomplete
            multiple
            freeSolo
            options={[]}
            value={values.query_names}
            onChange={(_event, next) => update('query_names', next.slice(0, MAX_QUERY_NAMES))}
            renderInput={(params) => (
              <TextField
                {...params}
                label={t('entities.httpActionRule.form.queryNamesLabel', 'Query parameter names')}
                helperText={t('entities.httpActionRule.form.queryNamesHelp', 'The query parameters a pipeline may set, up to 32. Type a name and press Enter.')}
              />
            )}
            data-testid="http-action-rule-form-query-names"
          />
          <SingleSelect
            label={t('entities.httpActionRule.form.credentialLabel', 'Credential')}
            value={String(values.configuration_id)}
            options={credentialOptions}
            onChange={(next) => update('configuration_id', Number(next))}
          />
          {textField('toolkit', t('entities.httpActionRule.form.toolkitLabel', 'Toolkit *'), 'crm')}
          {textField('tool', t('entities.httpActionRule.form.toolLabel', 'Tool *'), 'lookup_customer')}
          {textField('output_bucket', t('entities.httpActionRule.form.outputBucketLabel', 'Output bucket'), '')}
          <Box>
            <SingleSelect
              label={t('entities.httpActionRule.form.replayLabel', 'Replay')}
              value={values.replay}
              options={[
                { value: 'never', label: t('entities.httpActionRule.replay.never', 'Never') },
                { value: 'idempotency_key', label: t('entities.httpActionRule.replay.idempotencyKey', 'Idempotency key') },
              ]}
              onChange={(next) => update('replay', next as HttpActionRuleFormValues['replay'])}
            />
            <Typography variant="bodySmall" component="p" sx={helperSx}>
              {t('entities.httpActionRule.form.replayHelp', 'Choose Idempotency key only if the remote honours the Idempotency-Key header; otherwise an unknown outcome is reconciled, never repeated.')}
            </Typography>
          </Box>
          <FormControlLabel
            control={(
              <Switch
                checked={values.enabled}
                onChange={(event) => update('enabled', event.target.checked)}
                data-testid="http-action-rule-form-enabled"
              />
            )}
            label={t('entities.httpActionRule.form.enabledLabel', 'Enabled')}
          />
        </Box>
      }
      actions={{
        confirming: isSaving,
        confirmText: isEdit ? t('entities.httpActionRule.form.save', 'Save') : t('entities.httpActionRule.form.create', 'Create'),
        cancelText: t('entities.httpActionRule.form.cancel', 'Cancel'),
      }}
      onConfirm={handleSubmit}
      data-testid="http-action-rule-form-dialog"
    />
  );
}
