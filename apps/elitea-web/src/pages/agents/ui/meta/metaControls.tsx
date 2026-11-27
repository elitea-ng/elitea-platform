/**
 * The shared pieces of the agent editor's meta block controls (issue 3137):
 * one block's read/write handle over the editor's version state, and the
 * labelled number, select and switch fields every section uses. Each field
 * carries its label, its helper text and, when Main would refuse the value,
 * the reason in place of the helper.
 */
import { useState, type ReactNode } from 'react';

import FormControlLabel from '@mui/material/FormControlLabel';
import MenuItem from '@mui/material/MenuItem';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';

import { numberInput, withMetaValue, type MetaBlock, type MetaBlockKey } from '../../lib/agentMetaBlocks';
import type { EditApplicationVersionFieldsState } from '../../lib/useEditApplicationVersionFields';

type MetaVersionFields = Pick<EditApplicationVersionFieldsState, 'fields' | 'setMetaBlock' | 'metaProblems'>;

export interface MetaSectionProps {
  readonly versionFields: MetaVersionFields;
  readonly disabled?: boolean | undefined;
}

export interface MetaBlockHandle {
  readonly block: MetaBlock | null;
  readonly get: (path: readonly string[]) => unknown;
  readonly set: (path: readonly string[], value: unknown) => void;
  readonly problem: (path: readonly string[]) => string | undefined;
}

function at(block: MetaBlock | null, path: readonly string[]): unknown {
  let value: unknown = block;
  for (const key of path) value = typeof value === 'object' && value !== null ? (value as Record<string, unknown>)[key] : undefined;
  return value;
}

export function useMetaBlock(versionFields: MetaVersionFields, key: MetaBlockKey): MetaBlockHandle {
  const block = versionFields.fields.metaBlocks?.[key] ?? null;
  return {
    block,
    get: (path) => at(block, path),
    set: (path, value) => versionFields.setMetaBlock(key, withMetaValue(block, path, value)),
    problem: (path) => versionFields.metaProblems.get([key, ...path].join('.')),
  };
}

interface FieldProps {
  readonly handle: MetaBlockHandle;
  readonly path: readonly string[];
  readonly label: string;
  readonly helper?: string | undefined;
  readonly disabled?: boolean | undefined;
  readonly testId: string;
}

function textOf(value: unknown): string {
  return typeof value === 'number' || typeof value === 'string' ? String(value) : '';
}

/**
 * A number; empty removes the key (the default applies). Raw text that is not a number is kept, and refused.
 * While the field is focused it shows what was typed, so "0." stays "0." on the way to "0.5" (the stored value
 * is the parsed number, which has no trailing dot).
 */
export function MetaNumberField({ handle, path, label, helper, disabled, testId, placeholder }: FieldProps & { readonly placeholder?: string | undefined }): ReactNode {
  const problem = handle.problem(path);
  const [typed, setTyped] = useState<string | null>(null);
  return (
    <TextField
      label={label}
      value={typed ?? textOf(handle.get(path))}
      onChange={(event) => {
        setTyped(event.target.value);
        handle.set(path, numberInput(event.target.value));
      }}
      onBlur={() => setTyped(null)}
      error={problem !== undefined}
      helperText={problem ?? helper}
      disabled={disabled}
      placeholder={placeholder}
      size="small"
      slotProps={{ htmlInput: { inputMode: 'decimal', 'data-testid': testId } }}
    />
  );
}

/** Free text; empty removes the key. */
export function MetaTextField({ handle, path, label, helper, disabled, testId, placeholder }: FieldProps & { readonly placeholder?: string | undefined }): ReactNode {
  const problem = handle.problem(path);
  return (
    <TextField
      label={label}
      value={textOf(handle.get(path))}
      onChange={(event) => handle.set(path, event.target.value === '' ? undefined : event.target.value)}
      error={problem !== undefined}
      helperText={problem ?? helper}
      disabled={disabled}
      placeholder={placeholder}
      size="small"
      slotProps={{ htmlInput: { 'data-testid': testId } }}
    />
  );
}

export interface MetaOption {
  readonly value: string;
  readonly label: string;
}

/** One of `options`; the empty option removes the key (the default applies). */
export function MetaSelectField({ handle, path, label, helper, disabled, testId, options, defaultLabel }: FieldProps & { readonly options: readonly MetaOption[]; readonly defaultLabel: string }): ReactNode {
  const value = handle.get(path);
  return (
    <TextField
      select
      label={label}
      value={typeof value === 'string' ? value : ''}
      onChange={(event) => handle.set(path, event.target.value === '' ? undefined : event.target.value)}
      helperText={handle.problem(path) ?? helper}
      error={handle.problem(path) !== undefined}
      disabled={disabled}
      size="small"
      slotProps={{ htmlInput: { 'data-testid': testId }, inputLabel: { shrink: true }, select: { displayEmpty: true } }}
    >
      <MenuItem value="">{defaultLabel}</MenuItem>
      {options.map((option) => (
        <MenuItem key={option.value} value={option.value}>
          {option.label}
        </MenuItem>
      ))}
    </TextField>
  );
}

/** On or off; off removes the key unless `keepFalse` (a key whose absence means something else). */
export function MetaSwitchField({ handle, path, label, disabled, testId, keepFalse }: Omit<FieldProps, 'helper'> & { readonly keepFalse?: boolean }): ReactNode {
  return (
    <FormControlLabel
      label={label}
      disabled={disabled}
      control={
        <Switch
          checked={handle.get(path) === true}
          onChange={(_event, checked) => handle.set(path, checked || keepFalse === true ? checked : undefined)}
          slotProps={{ input: { 'data-testid': testId } as Record<string, string> }}
        />
      }
    />
  );
}
