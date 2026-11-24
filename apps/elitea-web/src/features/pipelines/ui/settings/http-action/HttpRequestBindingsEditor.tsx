import type { ReactNode } from 'react';

import AddIcon from '@mui/icons-material/Add';
import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlineOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormControlLabel from '@mui/material/FormControlLabel';
import IconButton from '@mui/material/IconButton';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { InputBase } from '@/shared/ui/InputBase';
import { SingleSelect, type SingleSelectOption } from '@/shared/ui/SingleSelect';

import {
  asRecord,
  readBindingMapRows,
  readBodyDraft,
  readHeaderRows,
  readIdempotencyDraft,
  writeBindingMapRows,
  writeBodyDraft,
  writeHeaderRows,
  writeIdempotencyDraft,
  type HttpBindingRow,
  type HttpBodyDraft,
  type HttpIdempotencyDraft,
} from './httpAction.helpers';

interface BindingSectionProps {
  readonly title: string;
  readonly keyPlaceholder: string;
  readonly rows: readonly HttpBindingRow[];
  readonly stateKeys: readonly string[];
  readonly disabled: boolean;
  readonly testIdPrefix: string;
  readonly onChange: (rows: readonly HttpBindingRow[]) => void;
}

const SOURCE_OPTIONS: readonly SingleSelectOption[] = [
  { value: 'from', label: 'State' },
  { value: 'value', label: 'Literal' },
];

const BODY_KIND_OPTIONS: readonly SingleSelectOption[] = [
  { value: 'none', label: 'None' },
  { value: 'json', label: 'JSON' },
  { value: 'text', label: 'Text' },
  { value: 'form', label: 'Form' },
];

const IDEMPOTENCY_OPTIONS: readonly SingleSelectOption[] = [
  { value: 'none', label: 'None' },
  { value: 'from', label: 'From state' },
  { value: 'value', label: 'Literal value' },
];

function BindingSection({
  title,
  keyPlaceholder,
  rows,
  stateKeys,
  disabled,
  testIdPrefix,
  onChange,
}: BindingSectionProps): ReactNode {
  const updateRow = (index: number, patch: Partial<HttpBindingRow>): void => {
    const next = rows.map((row, i) => (i === index ? { ...row, ...patch } : row));
    onChange(next);
  };
  const removeRow = (index: number): void => {
    onChange(rows.filter((_, i) => i !== index));
  };
  const addRow = (): void => {
    onChange([...rows, { name: '', source: 'from', value: '', optional: false }]);
  };

  return (
    <Stack spacing={1} className="nodrag nopan nowheel">
      <Stack direction="row" sx={{ justifyContent: 'space-between', alignItems: 'center' }}>
        <Typography variant="labelSmall" color="text.secondary">{title}</Typography>
        <Button
          size="small"
          variant="text"
          startIcon={<AddIcon fontSize="small" />}
          disabled={disabled || rows.length >= 32}
          data-testid={`pipelines-http-${testIdPrefix}-add`}
          onClick={addRow}
        >
          {t('pipelines.httpAction.addBinding', 'Add')}
        </Button>
      </Stack>
      {rows.map((row, index) => {
        const stateOptions: SingleSelectOption[] = [
          { value: '', label: t('pipelines.httpAction.selectState', 'Select state') },
          ...(!stateKeys.includes(row.value) && row.value !== '' ? [{ value: row.value, label: row.value }] : []),
          ...stateKeys.map((key) => ({ value: key, label: key })),
        ];
        return (
          <Stack key={`${testIdPrefix}-${index}`} spacing={0.75} sx={{ p: 1, borderRadius: ({ vars }) => vars.shape.radiusMd, border: ({ vars }) => `0.0625rem solid ${vars.palette.border.lines}` }}>
            <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
              <InputBase
                size="small"
                fullWidth
                label={keyPlaceholder}
                value={row.name}
                disabled={disabled}
                onChange={(event) => updateRow(index, { name: event.target.value })}
              />
              <Box sx={{ minWidth: '6.5rem' }}>
                <SingleSelect
                  label={t('pipelines.httpAction.source', 'Source')}
                  value={row.source}
                  disabled={disabled}
                  options={[...SOURCE_OPTIONS]}
                  onChange={(value) => updateRow(index, { source: value === 'value' ? 'value' : 'from', value: '', optional: false })}
                />
              </Box>
              <IconButton
                size="small"
                aria-label={t('pipelines.httpAction.removeBinding', 'Remove binding')}
                disabled={disabled}
                onClick={() => removeRow(index)}
              >
                <DeleteOutlineIcon fontSize="small" />
              </IconButton>
            </Stack>
            {row.source === 'from' ? (
              <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
                <Box sx={{ flex: 1 }}>
                  <SingleSelect
                    label={t('pipelines.httpAction.stateVariable', 'State variable')}
                    value={row.value}
                    disabled={disabled}
                    options={stateOptions}
                    onChange={(value) => updateRow(index, { value })}
                  />
                </Box>
                <FormControlLabel
                  control={
                    <BaseCheckbox
                      size="small"
                      checked={row.optional}
                      disabled={disabled}
                      onChange={(event) => updateRow(index, { optional: event.target.checked })}
                    />
                  }
                  label={<Typography variant="labelSmall">{t('pipelines.httpAction.optional', 'Optional')}</Typography>}
                />
              </Stack>
            ) : (
              <InputBase
                size="small"
                fullWidth
                label={t('pipelines.httpAction.literalValue', 'Literal value')}
                value={row.value}
                disabled={disabled}
                onChange={(event) => updateRow(index, { value: event.target.value })}
              />
            )}
          </Stack>
        );
      })}
    </Stack>
  );
}

export interface HttpRequestBindingsEditorProps {
  readonly request: unknown;
  readonly stateKeys: readonly string[];
  readonly disabled: boolean;
  readonly onChange: (nextRequest: Record<string, unknown>) => void;
}

export function HttpRequestBindingsEditor({
  request,
  stateKeys,
  disabled,
  onChange,
}: HttpRequestBindingsEditorProps): ReactNode {
  const rec = asRecord(request);
  const pathRows = readBindingMapRows(rec['path']);
  const queryRows = readBindingMapRows(rec['query']);
  const headerRows = readHeaderRows(rec['headers']);
  const body = readBodyDraft(rec['body']);
  const idempotency = readIdempotencyDraft(rec['idempotency_key']);

  const patchRequest = (field: string, value: unknown): void => {
    const next: Record<string, unknown> = { ...rec };
    if (value === undefined) delete next[field];
    else next[field] = value;
    onChange(next);
  };

  const updateBody = (patch: Partial<HttpBodyDraft>): void => {
    patchRequest('body', writeBodyDraft({ ...body, ...patch }));
  };

  const updateIdempotency = (patch: Partial<HttpIdempotencyDraft>): void => {
    patchRequest('idempotency_key', writeIdempotencyDraft({ ...idempotency, ...patch }));
  };

  const stateOptions: SingleSelectOption[] = [
    { value: '', label: t('pipelines.httpAction.selectState', 'Select state') },
    ...stateKeys.map((key) => ({ value: key, label: key })),
  ];

  return (
    <Stack spacing={1.5} className="nodrag nopan nowheel">
      <BindingSection
        title={t('pipelines.httpAction.pathParams', 'Path parameters')}
        keyPlaceholder={t('pipelines.httpAction.paramName', 'Name')}
        rows={pathRows}
        stateKeys={stateKeys}
        disabled={disabled}
        testIdPrefix="path"
        onChange={(rows) => patchRequest('path', writeBindingMapRows(rows))}
      />
      <BindingSection
        title={t('pipelines.httpAction.queryParams', 'Query parameters')}
        keyPlaceholder={t('pipelines.httpAction.paramName', 'Name')}
        rows={queryRows}
        stateKeys={stateKeys}
        disabled={disabled}
        testIdPrefix="query"
        onChange={(rows) => patchRequest('query', writeBindingMapRows(rows))}
      />
      <BindingSection
        title={t('pipelines.httpAction.headers', 'Headers')}
        keyPlaceholder={t('pipelines.httpAction.headerName', 'Header name')}
        rows={headerRows}
        stateKeys={stateKeys}
        disabled={disabled}
        testIdPrefix="headers"
        onChange={(rows) => patchRequest('headers', writeHeaderRows(rows))}
      />
      <Stack spacing={1}>
        <SingleSelect
          label={t('pipelines.httpAction.bodyKind', 'Request body')}
          value={body.kind}
          disabled={disabled}
          options={[...BODY_KIND_OPTIONS]}
          onChange={(value) => updateBody({ kind: value as HttpBodyDraft['kind'] })}
        />
        {body.kind !== 'none' && (
          <Stack spacing={1}>
            <SingleSelect
              label={t('pipelines.httpAction.bodySource', 'Body source')}
              value={body.source}
              disabled={disabled}
              options={[...SOURCE_OPTIONS]}
              onChange={(value) => updateBody({ source: value === 'value' ? 'value' : 'from', value: '' })}
            />
            {body.source === 'from' ? (
              <SingleSelect
                label={t('pipelines.httpAction.bodyStateKey', 'Body state variable')}
                value={body.value}
                disabled={disabled}
                options={
                  !stateKeys.includes(body.value) && body.value !== ''
                    ? [...stateOptions, { value: body.value, label: body.value }]
                    : stateOptions
                }
                onChange={(value) => updateBody({ value })}
              />
            ) : (
              <InputBase
                size="small"
                fullWidth
                label={t('pipelines.httpAction.bodyLiteral', 'Literal body')}
                value={body.value}
                disabled={disabled}
                expand={{ minRows: 2, maxRows: 6 }}
                onChange={(event) => updateBody({ value: event.target.value })}
              />
            )}
            <InputBase
              size="small"
              fullWidth
              label={t('pipelines.httpAction.contentType', 'Content-Type (optional)')}
              value={body.contentType}
              disabled={disabled}
              onChange={(event) => updateBody({ contentType: event.target.value })}
            />
          </Stack>
        )}
      </Stack>
      <Stack spacing={1}>
        <SingleSelect
          label={t('pipelines.httpAction.idempotencyKey', 'Idempotency key')}
          value={idempotency.mode}
          disabled={disabled}
          options={[...IDEMPOTENCY_OPTIONS]}
          onChange={(value) => updateIdempotency({ mode: value as HttpIdempotencyDraft['mode'], value: '' })}
        />
        {idempotency.mode === 'from' && (
          <SingleSelect
            label={t('pipelines.httpAction.idempotencyStateKey', 'Idempotency state variable')}
            value={idempotency.value}
            disabled={disabled}
            options={
              !stateKeys.includes(idempotency.value) && idempotency.value !== ''
                ? [...stateOptions, { value: idempotency.value, label: idempotency.value }]
                : stateOptions
            }
            onChange={(value) => updateIdempotency({ value })}
          />
        )}
        {idempotency.mode === 'value' && (
          <InputBase
            size="small"
            fullWidth
            label={t('pipelines.httpAction.idempotencyLiteral', 'Idempotency literal value')}
            value={idempotency.value}
            disabled={disabled}
            onChange={(event) => updateIdempotency({ value: event.target.value })}
          />
        )}
      </Stack>
    </Stack>
  );
}
