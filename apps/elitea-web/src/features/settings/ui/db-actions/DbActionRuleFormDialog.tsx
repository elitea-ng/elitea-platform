/**
 * DbActionRuleFormDialog — create or edit one project DB action rule (T-5E 5e-2),
 * mirroring `HttpActionRuleFormDialog`.
 *
 * The server owns validation (rule schema, SQL configuration access/dialect,
 * egress allowlist, uniqueness, the 256-rule cap); the form blocks obvious
 * blank required fields (including `configuration_id` > 0 and `marker_table`
 * when `mode === 'write'`). Switching `mode` to `'write'` pre-fills the default
 * marker table (`elitea_effects`); switching to `'read'` clears and hides it.
 */
import { useEffect, useState } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import Switch from '@mui/material/Switch';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { DbActionRuleWriteRequest } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { BaseModal } from '@/shared/ui/BaseModal';
import { InputBase } from '@/shared/ui/InputBase';
import { SingleSelect } from '@/shared/ui/SingleSelect';

import { useSqlCredentialOptions } from '../../lib/db-actions/useSqlCredentialOptions';

/** The write body minus `revision`, with `marker_table` concrete for inputs. */
type DbActionRuleFormValues = Omit<DbActionRuleWriteRequest, 'revision' | 'marker_table'> & {
  marker_table: string;
};

export interface DbActionRuleFormDialogProps {
  readonly open: boolean;
  readonly projectId: string;
  readonly isSaving: boolean;
  /** Present when editing an existing rule; absent for create. */
  readonly initialValues?: DbActionRuleFormValues | undefined;
  /** The server's refusal of the LAST submit (400 field/egress error, 409 conflict). */
  readonly serverError?: string | undefined;
  readonly onClose: () => void;
  readonly onSubmit: (values: DbActionRuleFormValues) => void;
}

const DEFAULT_MARKER_TABLE = 'elitea_effects';

const EMPTY_VALUES: DbActionRuleFormValues = {
  name: '',
  configuration_id: 0,
  dialect: 'postgres',
  database_name: '',
  mode: 'read',
  marker_table: '',
  enabled: true,
};

const contentSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1.25rem', minWidth: '25rem' };
const helperSx: SxProps<Theme> = { color: 'text.secondary', marginTop: '0.25rem' };
const errorHelperSx: SxProps<Theme> = { color: 'error.main', marginTop: '0.25rem' };

export function DbActionRuleFormDialog({ open, projectId, isSaving, initialValues, serverError, onClose, onSubmit }: DbActionRuleFormDialogProps) {
  const [values, setValues] = useState<DbActionRuleFormValues>(EMPTY_VALUES);
  const [missing, setMissing] = useState<ReadonlySet<string>>(new Set());
  const [serverErrorDismissed, setServerErrorDismissed] = useState(false);
  const credentialOptions = useSqlCredentialOptions(projectId, open, values.configuration_id);

  useEffect(() => {
    if (!open) return;
    setValues(initialValues ?? EMPTY_VALUES);
    setMissing(new Set());
    setServerErrorDismissed(false);
  }, [open, initialValues]);

  useEffect(() => {
    setServerErrorDismissed(false);
  }, [serverError]);

  const update = <K extends keyof DbActionRuleFormValues>(key: K, value: DbActionRuleFormValues[K]) => {
    setValues((previous) => {
      const next = { ...previous, [key]: value };
      if (key === 'mode') {
        if (value === 'write' && previous.marker_table.trim() === '') {
          next.marker_table = DEFAULT_MARKER_TABLE;
        } else if (value === 'read') {
          next.marker_table = '';
        }
      }
      return next;
    });
    setMissing((previous) => (previous.has(key) ? new Set([...previous].filter((entry) => entry !== key)) : previous));
    setServerErrorDismissed(true);
  };

  const handleSubmit = () => {
    const trimmed: DbActionRuleFormValues = {
      ...values,
      name: values.name.trim(),
      database_name: values.database_name.trim(),
      marker_table: values.mode === 'write' ? values.marker_table.trim() : '',
    };
    const blank: string[] = [];
    if (trimmed.name === '') blank.push('name');
    if (trimmed.configuration_id <= 0) blank.push('configuration_id');
    if (trimmed.database_name === '') blank.push('database_name');
    if (trimmed.mode === 'write' && trimmed.marker_table === '') blank.push('marker_table');
    if (blank.length > 0) {
      setMissing(new Set(blank));
      return;
    }
    onSubmit(trimmed);
  };

  const isEdit = initialValues !== undefined;
  const shownServerError = !serverErrorDismissed && serverError ? serverError : '';

  return (
    <BaseModal
      open={open}
      onClose={onClose}
      title={isEdit
        ? t('entities.dbActionRule.form.editTitle', 'Edit DB action rule')
        : t('entities.dbActionRule.form.createTitle', 'New DB action rule')}
      content={
        <Box sx={contentSx}>
          {shownServerError && <BannerMessage variant="error" message={shownServerError} />}
          <InputBase
            label={t('entities.dbActionRule.form.nameLabel', 'Name *')}
            placeholder="warehouse_pg.read"
            value={values.name}
            onChange={(event) => update('name', event.target.value)}
            error={missing.has('name')}
            helperText={missing.has('name') ? t('entities.dbActionRule.form.required', 'This field is required') : undefined}
            data-testid="db-action-rule-form-name"
          />
          <Box>
            <SingleSelect
              label={t('entities.dbActionRule.form.credentialLabel', 'SQL credential *')}
              value={String(values.configuration_id)}
              options={credentialOptions}
              onChange={(next) => update('configuration_id', Number(next))}
            />
            {missing.has('configuration_id') ? (
              <Typography variant="bodySmall" component="p" sx={errorHelperSx}>
                {t('entities.dbActionRule.form.credentialRequired', 'Select a SQL configuration')}
              </Typography>
            ) : (
              <Typography variant="bodySmall" component="p" sx={helperSx}>
                {t('entities.dbActionRule.form.credentialHelp', 'Host, port, username and password live only in the referenced SQL configuration.')}
              </Typography>
            )}
          </Box>
          <SingleSelect
            label={t('entities.dbActionRule.form.dialectLabel', 'Dialect')}
            value={values.dialect}
            options={[
              { value: 'postgres', label: t('entities.dbActionRule.dialect.postgres', 'PostgreSQL') },
              { value: 'mysql', label: t('entities.dbActionRule.dialect.mysql', 'MySQL') },
            ]}
            onChange={(next) => update('dialect', next as DbActionRuleFormValues['dialect'])}
          />
          <InputBase
            label={t('entities.dbActionRule.form.databaseLabel', 'Database *')}
            placeholder="analytics"
            value={values.database_name}
            onChange={(event) => update('database_name', event.target.value)}
            error={missing.has('database_name')}
            helperText={missing.has('database_name') ? t('entities.dbActionRule.form.required', 'This field is required') : undefined}
            data-testid="db-action-rule-form-database-name"
          />
          <Box>
            <SingleSelect
              label={t('entities.dbActionRule.form.modeLabel', 'Mode')}
              value={values.mode}
              options={[
                { value: 'read', label: t('entities.dbActionRule.mode.read', 'Read-only') },
                { value: 'write', label: t('entities.dbActionRule.mode.write', 'Write (transactional)') },
              ]}
              onChange={(next) => update('mode', next as DbActionRuleFormValues['mode'])}
            />
            <Typography variant="bodySmall" component="p" sx={helperSx}>
              {t('entities.dbActionRule.form.modeHelp', 'Read-only rules run inside a READ ONLY transaction. Write rules require an effect marker table in the target database.')}
            </Typography>
          </Box>
          {values.mode === 'write' && (
            <InputBase
              label={t('entities.dbActionRule.form.markerTableLabel', 'Marker table *')}
              placeholder={DEFAULT_MARKER_TABLE}
              value={values.marker_table}
              onChange={(event) => update('marker_table', event.target.value)}
              error={missing.has('marker_table')}
              helperText={
                missing.has('marker_table')
                  ? t('entities.dbActionRule.form.required', 'This field is required')
                  : t('entities.dbActionRule.form.markerTableHelp', 'Table or schema.table created in the target database to reconcile write outcomes (default: elitea_effects).')
              }
              data-testid="db-action-rule-form-marker-table"
            />
          )}
          <FormControlLabel
            control={(
              <Switch
                checked={values.enabled}
                onChange={(event) => update('enabled', event.target.checked)}
                data-testid="db-action-rule-form-enabled"
              />
            )}
            label={t('entities.dbActionRule.form.enabledLabel', 'Enabled')}
          />
        </Box>
      }
      actions={{
        confirming: isSaving,
        confirmText: isEdit ? t('entities.dbActionRule.form.save', 'Save') : t('entities.dbActionRule.form.create', 'Create'),
        cancelText: t('entities.dbActionRule.form.cancel', 'Cancel'),
      }}
      onConfirm={handleSubmit}
      data-testid="db-action-rule-form-dialog"
    />
  );
}
