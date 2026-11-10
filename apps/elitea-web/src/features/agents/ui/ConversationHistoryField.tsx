import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import FormHelperText from '@mui/material/FormHelperText';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';
import { InfoLabelWithTooltip } from '@/shared/ui/InfoLabelWithTooltip';

import {
  CONTEXT_HISTORY_MODES,
  contextBlockForSave,
  readContextBlock,
  type ContextBlock,
  type ContextHistoryMode,
  type ContextSideTalk,
} from '../lib/contextBlock';

function historyLabel(mode: ContextHistoryMode): string {
  switch (mode) {
    case 'since_joined':
      return t('features.agents.contextHistory.sinceJoined', 'Since it joined');
    case 'addressed_only':
      return t('features.agents.contextHistory.addressedOnly', 'Only messages addressed to it');
    case 'none':
      return t('features.agents.contextHistory.none', 'None (the current message only)');
    case 'full':
      return t('features.agents.contextHistory.full', 'Everything it may see');
  }
}

/**
 * "Conversation history": what this agent sees of a conversation it shares
 * with people and other agents (ADR-0050 decision 9). Writes `meta.context`;
 * the default (everything, side talk it is a party to) clears the key.
 * `error` is the server's refusal of the block (422 `context_block_invalid`),
 * shown on the field.
 */
export function ConversationHistoryField({ value, onChange, disabled, error }: {
  readonly value: unknown;
  readonly onChange: (next: ContextBlock | null) => void;
  readonly disabled?: boolean | undefined;
  readonly error?: string | undefined;
}): ReactNode {
  const block = readContextBlock(value) ?? { history: 'full', include_side_talk: 'party' };
  const update = (patch: Partial<ContextBlock>): void => onChange(contextBlockForSave({ ...block, ...patch }));
  const label = t('features.agents.contextHistory.label', 'Conversation history');
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }} data-testid="agent-context-history">
      <InfoLabelWithTooltip
        label={label}
        tooltip={t(
          'features.agents.contextHistory.tooltip',
          'Which messages of a shared conversation this agent reads when it answers. It never sees another agent\'s tool calls or thinking, and it reads side talk only when it is a party to it.',
        )}
        variant="bodyMedium"
      />
      <TextField
        select
        size="small"
        value={block.history}
        disabled={disabled}
        error={error !== undefined}
        onChange={(event) => update({ history: event.target.value as ContextHistoryMode })}
        slotProps={{ htmlInput: { 'aria-label': label, 'data-testid': 'agent-context-history-mode' } }}
      >
        {CONTEXT_HISTORY_MODES.map((mode) => (
          <MenuItem key={mode} value={mode} data-testid={`agent-context-history-${mode}`}>
            {historyLabel(mode)}
          </MenuItem>
        ))}
      </TextField>
      <TextField
        select
        size="small"
        value={block.include_side_talk}
        disabled={disabled}
        error={error !== undefined}
        onChange={(event) => update({ include_side_talk: event.target.value as ContextSideTalk })}
        slotProps={{
          htmlInput: { 'aria-label': t('features.agents.contextHistory.sideTalkLabel', 'Side talk'), 'data-testid': 'agent-context-side-talk' },
        }}
      >
        <MenuItem value="party">{t('features.agents.contextHistory.sideTalkParty', 'Side talk it is a party to')}</MenuItem>
        <MenuItem value="none">{t('features.agents.contextHistory.sideTalkNone', 'No side talk')}</MenuItem>
      </TextField>
      {error !== undefined && (
        <FormHelperText error data-testid="agent-context-history-error">
          {error}
        </FormHelperText>
      )}
    </Box>
  );
}
