import type { ChangeEvent, ReactNode } from 'react';
import Box from '@mui/material/Box';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { InfoLabelWithTooltip } from '@/shared/ui/InfoLabelWithTooltip';

/** `meta.citations` (ADR-0043 decision 5): `'required'` or absent. */
export type CitationsValue = 'required' | null;

/** The stored `meta.citations` as the field's value; anything but `'required'` is off. */
function readCitations(raw: unknown): CitationsValue {
  return raw === 'required' ? 'required' : null;
}

/**
 * "Require citations": the agent cites the tool results it uses as [n], and
 * the chat shows how many sources the answer cites and which statements have
 * no citation. Off clears the key.
 */
export function CitationsField({ value, onChange, disabled }: {
  readonly value: unknown;
  readonly onChange: (next: CitationsValue) => void;
  readonly disabled?: boolean | undefined;
}): ReactNode {
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
      <BaseCheckbox
        checked={readCitations(value) === 'required'}
        onChange={(event: ChangeEvent<HTMLInputElement>) => onChange(event.target.checked ? 'required' : null)}
        disabled={disabled}
        slotProps={{ input: { 'aria-label': t('features.agents.citations.label', 'Require citations') } }}
        data-testid="agent-citations"
      />
      <InfoLabelWithTooltip
        label={t('features.agents.citations.label', 'Require citations')}
        tooltip={t(
          'features.agents.citations.tooltip',
          'The agent cites the tool results it uses as [1], [2]. The chat shows how many sources an answer cites and which statements have no citation. It is not a confidence score.',
        )}
        variant="bodyMedium"
      />
    </Box>
  );
}
