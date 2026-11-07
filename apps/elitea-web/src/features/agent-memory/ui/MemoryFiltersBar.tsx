/**
 * The Memory tab's search and filters: kind, status, the person an entry is
 * about (only the people the caller can see, from the list's `people`), who
 * wrote it, and a created-at date range.
 */
import type { ChangeEvent, ReactNode } from 'react';

import Box from '@mui/material/Box';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';

import type { MemoryFilters } from '../lib/memoryFilters';
import { kindLabel, statusLabel } from './MemoryEditDialog';

export interface MemoryFiltersBarProps {
  readonly filters: MemoryFilters;
  readonly people: readonly { readonly id: number; readonly name: string }[];
  readonly onChange: (filters: MemoryFilters) => void;
}

interface SelectSpec {
  readonly key: 'kind' | 'status' | 'personId' | 'source';
  readonly label: string;
  readonly options: readonly { readonly value: string; readonly label: string }[];
}

function selects(people: MemoryFiltersBarProps['people']): readonly SelectSpec[] {
  return [
    {
      key: 'kind',
      label: t('features.agentMemory.filters.kind', 'Kind'),
      options: ['fact', 'preference', 'episode', 'cursor'].map((value) => ({ value, label: kindLabel(value) })),
    },
    {
      key: 'status',
      label: t('features.agentMemory.filters.status', 'Status'),
      options: ['pending', 'active', 'archived'].map((value) => ({ value, label: statusLabel(value) })),
    },
    {
      key: 'personId',
      label: t('features.agentMemory.filters.person', 'About'),
      options: people.map((person) => ({ value: String(person.id), label: person.name })),
    },
    {
      key: 'source',
      label: t('features.agentMemory.filters.source', 'Written by'),
      options: [
        { value: 'agent', label: t('features.agentMemory.writer.agent', 'The agent') },
        { value: 'person', label: t('features.agentMemory.writer.person', 'A person') },
      ],
    },
  ];
}

const selectSx = { minWidth: '9rem' };

export function MemoryFiltersBar({ filters, people, onChange }: MemoryFiltersBarProps): ReactNode {
  const set = (patch: Partial<MemoryFilters>): void => onChange({ ...filters, ...patch });
  return (
    <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: '0.75rem', alignItems: 'center' }} data-testid="agent-memory-filters">
      <SimpleSearchBar
        value={filters.query}
        onChange={(query) => set({ query })}
        placeholder={t('features.agentMemory.filters.search', 'Search memories')}
        aria-label={t('features.agentMemory.filters.search', 'Search memories')}
        data-testid="agent-memory-search"
      />
      {selects(people).map((spec) => (
        <TextField
          key={spec.key}
          select
          size="small"
          label={spec.label}
          value={filters[spec.key]}
          onChange={(event: ChangeEvent<HTMLInputElement>) => set({ [spec.key]: event.target.value })}
          sx={selectSx}
          slotProps={{ htmlInput: { 'data-testid': `agent-memory-filter-${spec.key}` } }}
        >
          <MenuItem value="">{t('features.agentMemory.filters.any', 'Any')}</MenuItem>
          {spec.options.map((option) => (
            <MenuItem key={option.value} value={option.value}>
              {option.label}
            </MenuItem>
          ))}
        </TextField>
      ))}
      <TextField
        type="date"
        size="small"
        label={t('features.agentMemory.filters.from', 'From')}
        value={filters.from}
        onChange={(event: ChangeEvent<HTMLInputElement>) => set({ from: event.target.value })}
        slotProps={{ inputLabel: { shrink: true }, htmlInput: { 'data-testid': 'agent-memory-filter-from' } }}
      />
      <TextField
        type="date"
        size="small"
        label={t('features.agentMemory.filters.to', 'To')}
        value={filters.to}
        onChange={(event: ChangeEvent<HTMLInputElement>) => set({ to: event.target.value })}
        slotProps={{ inputLabel: { shrink: true }, htmlInput: { 'data-testid': 'agent-memory-filter-to' } }}
      />
    </Box>
  );
}
