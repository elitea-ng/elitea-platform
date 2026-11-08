/**
 * Agent-to-agent talk and platform lines in the transcript (ADR-0050
 * decision 7e, issue 1612).
 *
 * A run of consecutive agent-to-agent rows folds into one collapsed
 * "Side conversation (n)" chip (`foldSideTalk`, `@elitea/client-core`);
 * expanding it shows each row compactly, labelled "author → addressees". A
 * platform line (a loop guard, a hand-off) is centred and muted, with no
 * avatar.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';
import ExpandLessIcon from '@mui/icons-material/ExpandLess';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';

import { MESSAGE_CLASS, foldSideTalk, sideTalkLabel, type FoldedEntry } from '@elitea/client-core/conversation';

import { t } from '@/shared/i18n';

import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';

/** A transcript row with the index it has in the unfolded history. */
export interface IndexedRow {
  readonly messageClass: NonNullable<ChatMessage['messageClass']>;
  readonly message: ChatMessage;
  readonly index: number;
}

/** The transcript with its side talk folded. A row with no class (a person's, a live one) is never folded. */
export function foldTranscript(chatHistory: readonly ChatMessage[]): FoldedEntry<IndexedRow>[] {
  const rows = chatHistory.map((message, index) => ({ messageClass: message.messageClass ?? MESSAGE_CLASS.Human, message, index }));
  return foldSideTalk(rows, (row) => row.message.id);
}

/** Whether a row is a platform line. */
function isSystemLine(message: ChatMessage): boolean {
  return message.messageClass === MESSAGE_CLASS.System;
}

function SystemLine({ message }: { readonly message: ChatMessage }): ReactNode {
  return (
    <Box data-testid="chat-system-line" sx={{ display: 'flex', justifyContent: 'center', paddingX: 2 }}>
      <Typography variant="bodySmall" color="text.secondary" sx={{ textAlign: 'center', fontStyle: 'italic' }}>
        {message.content}
      </Typography>
    </Box>
  );
}

function SideTalkRow({ message }: { readonly message: ChatMessage }): ReactNode {
  const author = message.authorName ?? '';
  return (
    <Box data-testid="chat-side-talk-row" sx={{ display: 'flex', flexDirection: 'column', gap: 0.25 }}>
      <Typography variant="labelSmall" color="text.secondary" data-testid="chat-side-talk-label">
        {sideTalkLabel(author, message.addresseeNames ?? [])}
      </Typography>
      <Typography variant="bodySmall" color="text.primary" sx={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
        {message.content}
      </Typography>
    </Box>
  );
}

function SideTalk({ rows }: { readonly rows: readonly IndexedRow[] }): ReactNode {
  const [open, setOpen] = useState(false);
  const label = t('chatMessages.sideTalk.chip', 'Side conversation ({{n}})', { n: rows.length });
  return (
    <Box data-testid="chat-side-talk" sx={{ display: 'flex', flexDirection: 'column', gap: 1, paddingLeft: 5 }}>
      <ButtonBase
        onClick={() => setOpen((prev) => !prev)}
        aria-expanded={open}
        data-testid="chat-side-talk-chip"
        sx={(theme: Theme) => ({
          alignSelf: 'flex-start',
          gap: 0.5,
          paddingX: 1.5,
          paddingY: 0.5,
          borderRadius: theme.vars.shape.radiusLg,
          border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
          color: theme.vars.palette.text.secondary,
        })}
      >
        <Typography variant="labelSmall" color="inherit">{label}</Typography>
        {open ? <ExpandLessIcon fontSize="small" /> : <ExpandMoreIcon fontSize="small" />}
      </ButtonBase>
      {open && (
        <Box
          sx={(theme: Theme) => ({
            display: 'flex',
            flexDirection: 'column',
            gap: 1,
            paddingLeft: 1.5,
            borderLeft: `0.125rem solid ${theme.vars.palette.border.lines}`,
          })}
        >
          {rows.map((row) => <SideTalkRow key={row.message.id} message={row.message} />)}
        </Box>
      )}
    </Box>
  );
}

/**
 * One folded entry as list items: a side-talk run as its chip, a platform
 * line centred, any other row through `renderRow` with its history index.
 */
export function renderFolded(entry: FoldedEntry<IndexedRow>, renderRow: (message: ChatMessage, index: number) => ReactNode): ReactNode {
  if (entry.kind === 'side_talk') {
    return (
      <Box component="li" key={entry.key} data-testid="chat-message-item" sx={{ listStyle: 'none', width: '100%', mb: 2 }}>
        <SideTalk rows={entry.rows} />
      </Box>
    );
  }
  const { message, index } = entry.row;
  if (!isSystemLine(message)) return renderRow(message, index);
  return (
    <Box component="li" key={message.id} data-testid="chat-message-item" sx={{ listStyle: 'none', width: '100%', mb: 2 }}>
      <SystemLine message={message} />
    </Box>
  );
}
