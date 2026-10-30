import type { ChangeEvent, ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import type { OutputContract } from '@/shared/api/generated/model';
import { outputContractPatternMax } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { InputBase } from '../InputBase';
import { SingleSelect } from '../SingleSelect';
import {
  fromOutputContractDraft,
  toOutputContractDraft,
  type OutputContractDraft,
  type OutputContractFormat,
  type OutputContractKind,
  type OutputContractParse,
} from './outputContract';

/**
 * The output contract editor (ADR-0043 decision 2, issue #1500): a type
 * selector and one input for that type. The agent editor's Advanced panel
 * and the pipeline LLM node use it.
 *
 * The field holds its own draft. A JSON Schema is text while the person
 * types, and text that does not parse is not sent up: `onChange` fires only
 * with a contract the server can read (or `null` for None). The parse error
 * shows under the text area. The server does the full check on save.
 *
 * The draft is re-seeded when `value` changes from outside (a version switch,
 * a YAML edit). The value this field sent itself does not re-seed it, so the
 * person's own JSON formatting stays.
 */
export interface OutputContractFieldProps {
  readonly value: OutputContract | null | undefined;
  readonly onChange: (next: OutputContract | null) => void;
  readonly disabled?: boolean | undefined;
  /** Prefix of the test ids. Default `output-contract`. */
  readonly testIdPrefix?: string | undefined;
}

const SCHEMA_STARTER = '{\n  "type": "object"\n}';

function kindOptions() {
  return [
    { value: 'none', label: t('shared.ui.outputContractField.type.none', 'None') },
    { value: 'json_schema', label: t('shared.ui.outputContractField.type.jsonSchema', 'JSON Schema') },
    { value: 'regex', label: t('shared.ui.outputContractField.type.regex', 'Regular expression') },
    { value: 'format', label: t('shared.ui.outputContractField.type.format', 'Format') },
  ];
}

function formatOptions() {
  return [
    { value: 'json', label: t('shared.ui.outputContractField.format.json', 'JSON') },
    { value: 'markdown', label: t('shared.ui.outputContractField.format.markdown', 'Markdown') },
    { value: 'csv', label: t('shared.ui.outputContractField.format.csv', 'CSV') },
  ];
}

function parseErrorText(parse: OutputContractParse): string | undefined {
  if (parse.ok) return undefined;
  return parse.error === 'json'
    ? t('shared.ui.outputContractField.error.json', 'This text is not valid JSON. If you save now, the last valid schema stays.')
    : t('shared.ui.outputContractField.error.notObject', 'The schema must be a JSON object. If you save now, the last valid schema stays.');
}

function serialise(contract: OutputContract | null | undefined): string {
  return JSON.stringify(contract ?? null);
}

export function OutputContractField({ value, onChange, disabled, testIdPrefix = 'output-contract' }: OutputContractFieldProps): ReactNode {
  const incoming = serialise(value);
  const [seed, setSeed] = useState(incoming);
  const [draft, setDraft] = useState<OutputContractDraft>(() => toOutputContractDraft(value));
  // Render-phase re-seed, the same pattern `useEditApplicationVersionFields`
  // uses: an effect would paint one frame of the old draft first.
  if (seed !== incoming) {
    setSeed(incoming);
    setDraft(toOutputContractDraft(value));
  }

  const update = (next: OutputContractDraft) => {
    setDraft(next);
    const result = fromOutputContractDraft(next);
    if (!result.ok) return;
    const serialised = serialise(result.contract);
    if (serialised === seed) return;
    setSeed(serialised);
    onChange(result.contract);
  };

  const onKindChange = (kind: string) => {
    const schemaText = kind === 'json_schema' && draft.schemaText.trim() === '' ? SCHEMA_STARTER : draft.schemaText;
    update({ ...draft, kind: kind as OutputContractKind, schemaText });
  };
  const onSchemaChange = (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) =>
    update({ ...draft, schemaText: event.target.value });
  const onPatternChange = (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) =>
    update({ ...draft, pattern: event.target.value });
  const onFormatChange = (format: string) => update({ ...draft, format: format as OutputContractFormat });

  const schemaError = draft.kind === 'json_schema' ? parseErrorText(fromOutputContractDraft(draft)) : undefined;

  return (
    <Box sx={containerSx} data-testid={testIdPrefix}>
      <SingleSelect
        value={draft.kind}
        onChange={onKindChange}
        options={kindOptions()}
        label={t('shared.ui.outputContractField.label', 'Output contract')}
        disabled={disabled ?? false}
      />
      {draft.kind === 'json_schema' && (
        <InputBase
          value={draft.schemaText}
          onChange={onSchemaChange}
          label={t('shared.ui.outputContractField.schema.label', 'JSON Schema')}
          expand={{ minRows: 4, maxRows: 16 }}
          disabled={disabled}
          error={schemaError !== undefined}
          helperText={schemaError ?? t('shared.ui.outputContractField.schema.helper', 'The final answer must be JSON that agrees with this schema.')}
          slotProps={{ htmlInput: { 'data-testid': `${testIdPrefix}-schema`, spellCheck: false } }}
        />
      )}
      {draft.kind === 'regex' && (
        <InputBase
          value={draft.pattern}
          onChange={onPatternChange}
          label={t('shared.ui.outputContractField.pattern.label', 'Regular expression (RE2)')}
          disabled={disabled}
          helperText={t('shared.ui.outputContractField.pattern.helper', 'The expression must match all of the final answer.')}
          slotProps={{ htmlInput: { 'data-testid': `${testIdPrefix}-pattern`, maxLength: outputContractPatternMax, spellCheck: false } }}
        />
      )}
      {draft.kind === 'format' && (
        <SingleSelect
          value={draft.format}
          onChange={onFormatChange}
          options={formatOptions()}
          label={t('shared.ui.outputContractField.format.label', 'Format')}
          disabled={disabled ?? false}
        />
      )}
    </Box>
  );
}

const containerSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.75rem',
};
