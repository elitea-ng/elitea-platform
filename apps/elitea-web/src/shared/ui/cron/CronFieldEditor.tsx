/**
 * One cron field's editor: a kind toggle (Every / Specific / Range / Every N)
 * plus the value picker for whichever kind is active. Built entirely on MUI
 * primitives (`Select`, `Chip`, `ToggleButtonGroup`) per spec §9.3 unit S7 —
 * no `react-js-cron`, no `antd`.
 */
import type { JSX, MouseEvent } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import FormControl from '@mui/material/FormControl';
import FormLabel from '@mui/material/FormLabel';
import InputLabel from '@mui/material/InputLabel';
import MenuItem from '@mui/material/MenuItem';
import type { SelectChangeEvent } from '@mui/material/Select';
import Select from '@mui/material/Select';
import Stack from '@mui/material/Stack';
import ToggleButton from '@mui/material/ToggleButton';
import ToggleButtonGroup from '@mui/material/ToggleButtonGroup';

import { t } from '@/shared/i18n';
import type { CronFieldBounds, CronFieldId, CronFieldKind, CronFieldRange, CronFieldState } from './model';
import { rangeArray } from './model';

export interface CronFieldEditorProps {
  readonly fieldId: CronFieldId;
  readonly label: string;
  readonly bounds: CronFieldBounds;
  readonly state: CronFieldState;
  readonly onChange: (next: CronFieldState) => void;
  readonly disabled?: boolean;
  /** Weekday/month fields render names instead of raw numbers. */
  readonly optionLabel?: ((value: number) => string) | undefined;
}

const KIND_OPTIONS: readonly { readonly value: CronFieldKind; readonly key: string; readonly fallback: string }[] = [
  { value: 'every', key: 'shared.ui.cron.kind.every', fallback: 'Every' },
  { value: 'list', key: 'shared.ui.cron.kind.list', fallback: 'Specific' },
  { value: 'range', key: 'shared.ui.cron.kind.range', fallback: 'Range' },
  { value: 'step', key: 'shared.ui.cron.kind.step', fallback: 'Every N' },
];

function buildDefaultState(kind: CronFieldKind, bounds: CronFieldBounds): CronFieldState {
  switch (kind) {
    case 'every':
      return { kind: 'every' };
    case 'list':
      return { kind: 'list', values: [bounds.min] };
    case 'range':
      return { kind: 'range', from: bounds.min, to: Math.min(bounds.min + 1, bounds.max) };
    case 'step':
      return { kind: 'step', step: Math.min(bounds.defaultStep, bounds.max) };
  }
}

// Takes the current (already-narrowed) range state explicitly rather than
// re-deriving it from a union-typed `state` inside the handler — that would
// need a `state.kind !== 'range'` guard that JSX's conditional rendering
// already makes unreachable (these handlers are only ever wired to the
// Selects rendered inside the `state.kind === 'range'` branch below).
function nextRangeFrom(current: CronFieldRange, from: number): CronFieldRange {
  return { kind: 'range', from, to: Math.max(from, current.to) };
}
function nextRangeTo(current: CronFieldRange, to: number): CronFieldRange {
  return { kind: 'range', from: Math.min(current.from, to), to };
}

/**
 * `SelectChangeEvent<number[]>.target.value` types as `string | number[]`
 * even though a non-`native` multi-`Select` (what this component always
 * renders) only ever delivers the array shape at runtime — the `string`
 * half comes from `SelectChangeEvent`'s other union member,
 * `React.ChangeEvent<HTMLInputElement>`, whose `target.value` is always
 * `string` in the DOM regardless of the generic (`SelectInput.d.ts`).
 * Pulled out so the string branch — real per the *type*, unreachable via
 * this component's own DOM — is unit-testable without a `native` Select.
 */
export function normalizeSelectMultipleValue(raw: string | readonly number[]): number[] {
  const values = typeof raw === 'string' ? raw.split(',').filter(Boolean).map(Number) : [...raw];
  return values.sort((a, b) => a - b);
}

export function CronFieldEditor(props: CronFieldEditorProps): JSX.Element {
  const { fieldId, label, bounds, state, onChange, disabled = false, optionLabel } = props;
  const options = rangeArray(bounds.min, bounds.max);
  const formatOption = optionLabel ?? ((value: number) => String(value));
  const kindLabelId = `cron-${fieldId}-kind-label`;
  const valuesLabelId = `cron-${fieldId}-values-label`;
  const fromLabelId = `cron-${fieldId}-from-label`;
  const toLabelId = `cron-${fieldId}-to-label`;
  const stepLabelId = `cron-${fieldId}-step-label`;

  // `exclusive` ToggleButtonGroup reports `null` when the already-active
  // button is clicked again (deselect) — that is the only way MUI ever
  // calls this with a falsy value, so a no-op guard on it is enough; it
  // never reports the *same* kind as a truthy value (a different button was
  // necessarily clicked to get a truthy `nextKind` at all).
  const handleKindChange = (_event: MouseEvent<HTMLElement>, nextKind: CronFieldKind | null): void => {
    if (!nextKind) return;
    onChange(buildDefaultState(nextKind, bounds));
  };

  const handleListChange = (event: SelectChangeEvent<number[]>): void => {
    const values = normalizeSelectMultipleValue(event.target.value);
    if (values.length === 0) return;
    onChange({ kind: 'list', values });
  };

  const handleStepChange = (event: SelectChangeEvent<number>): void => {
    onChange({ kind: 'step', step: Number(event.target.value) });
  };

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      <FormLabel id={kindLabelId}>
        {label}
      </FormLabel>
      <ToggleButtonGroup
        size="small"
        exclusive
        value={state.kind}
        onChange={handleKindChange}
        aria-labelledby={kindLabelId}
        disabled={disabled}
      >
        {KIND_OPTIONS.map((option) => (
          <ToggleButton key={option.value} value={option.value}>
            {t(option.key, option.fallback)}
          </ToggleButton>
        ))}
      </ToggleButtonGroup>

      {state.kind === 'list' && (
        <FormControl size="small" fullWidth disabled={disabled}>
          <InputLabel id={valuesLabelId}>{t('shared.ui.cron.values', 'Values')}</InputLabel>
          <Select<number[]>
            labelId={valuesLabelId}
            id={`cron-${fieldId}-values`}
            multiple
            label={t('shared.ui.cron.values', 'Values')}
            value={state.values as number[]}
            onChange={handleListChange}
            renderValue={(selected) => (
              <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 0.5 }}>
                {selected.map((value) => (
                  <Chip key={value} size="small" label={formatOption(value)} />
                ))}
              </Box>
            )}
          >
            {options.map((value) => (
              <MenuItem key={value} value={value}>
                {formatOption(value)}
              </MenuItem>
            ))}
          </Select>
        </FormControl>
      )}

      {state.kind === 'range' && (
        <Stack direction="row" spacing={1}>
          <FormControl size="small" fullWidth disabled={disabled}>
            <InputLabel id={fromLabelId}>{t('shared.ui.cron.from', 'From')}</InputLabel>
            <Select<number>
              labelId={fromLabelId}
              id={`cron-${fieldId}-from`}
              label={t('shared.ui.cron.from', 'From')}
              value={state.from}
              onChange={(event) => onChange(nextRangeFrom(state, Number(event.target.value)))}
            >
              {options.map((value) => (
                <MenuItem key={value} value={value}>
                  {formatOption(value)}
                </MenuItem>
              ))}
            </Select>
          </FormControl>
          <FormControl size="small" fullWidth disabled={disabled}>
            <InputLabel id={toLabelId}>{t('shared.ui.cron.to', 'To')}</InputLabel>
            <Select<number>
              labelId={toLabelId}
              id={`cron-${fieldId}-to`}
              label={t('shared.ui.cron.to', 'To')}
              value={state.to}
              onChange={(event) => onChange(nextRangeTo(state, Number(event.target.value)))}
            >
              {options.map((value) => (
                <MenuItem key={value} value={value}>
                  {formatOption(value)}
                </MenuItem>
              ))}
            </Select>
          </FormControl>
        </Stack>
      )}

      {state.kind === 'step' && (
        <FormControl size="small" fullWidth disabled={disabled}>
          <InputLabel id={stepLabelId}>{t('shared.ui.cron.everyN', 'Every')}</InputLabel>
          <Select<number>
            labelId={stepLabelId}
            id={`cron-${fieldId}-step`}
            label={t('shared.ui.cron.everyN', 'Every')}
            value={state.step}
            onChange={handleStepChange}
          >
            {rangeArray(1, bounds.max).map((value) => (
              <MenuItem key={value} value={value}>
                {value}
              </MenuItem>
            ))}
          </Select>
        </FormControl>
      )}
    </Box>
  );
}
