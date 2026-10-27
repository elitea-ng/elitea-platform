import type { KeyboardEvent, ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

/** The streamed draft's last characters, so a person sees the model write it (ADR-0037). */
const PREVIEW_CHARS = 1200;

export function DraftStreamPreview({ text }: { readonly text: string }): ReactNode {
  const shown = text.length > PREVIEW_CHARS ? `…${text.slice(-PREVIEW_CHARS)}` : text;
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1.5, paddingY: 2 }}>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
        <CircularProgress size={18} />
        <Typography variant="bodyMedium" color="text.secondary">
          {t('features.agents.generateAgentModal.generating', 'Generating agent draft...')}
        </Typography>
      </Box>
      {shown !== '' && (
        <Typography
          component="pre"
          variant="bodySmall"
          data-testid="agent-draft-stream"
          aria-live="polite"
          sx={{ margin: 0, maxHeight: '16rem', overflow: 'auto', whiteSpace: 'pre-wrap', wordBreak: 'break-word', color: 'text.secondary' }}
        >
          {shown}
        </Typography>
      )}
    </Box>
  );
}

/** A follow-up request that revises the draft under review, as the next turn of the builder conversation. */
export function RefineDraftField({ disabled, onRefine }: { readonly disabled: boolean; readonly onRefine: (request: string) => void }): ReactNode {
  const [request, setRequest] = useState('');
  const send = (): void => {
    const trimmed = request.trim();
    if (trimmed === '' || disabled) return;
    onRefine(trimmed);
    setRequest('');
  };
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>): void => {
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault();
      send();
    }
  };
  return (
    <Box sx={{ display: 'flex', alignItems: 'flex-end', gap: 1, marginTop: 2 }}>
      <TextField
        fullWidth
        size="small"
        multiline
        maxRows={4}
        value={request}
        disabled={disabled}
        onChange={(event) => setRequest(event.target.value)}
        onKeyDown={onKeyDown}
        placeholder={t('features.agents.generateAgentModal.refinePlaceholder', 'Ask for a change, e.g. "Make it more formal"')}
        slotProps={{ htmlInput: { 'aria-label': t('features.agents.generateAgentModal.refineLabel', 'Change the draft') } }}
      />
      <BaseBtn variant="secondary" disabled={disabled || request.trim() === ''} onClick={send}>
        {t('features.agents.generateAgentModal.refine', 'Refine')}
      </BaseBtn>
    </Box>
  );
}
