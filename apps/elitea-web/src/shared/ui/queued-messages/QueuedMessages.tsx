/**
 * The messages that wait for the running turn, at the top of the composer — Chat and a Local work thread alike:
 * a muted "Queued" caption, then one quiet row per message, oldest first: its text on one line (the full text in
 * the row's title), and three small icons, the reply rows' size.
 *
 * - Send now: stop the current step and send this message first.
 * - Edit: take it off the queue and put it back into the message box.
 * - Remove: take it off the queue.
 *
 * What each does is the page's (`onSendNow`, `onEdit`, `onRemove`, by the row's `id`); this only draws the rows.
 * Nothing at all while the queue is empty.
 */
import type { ReactNode } from 'react';

import ArrowUpwardIcon from '@mui/icons-material/ArrowUpward';
import CloseIcon from '@mui/icons-material/Close';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

export interface QueuedMessage {
  readonly id: string;
  readonly text: string;
  /** The row's test id (default `queued-message`): a page tells its kinds of rows apart by it. */
  readonly testId?: string;
  /** The row's `data-state` (a steering message's `pending`). */
  readonly state?: string;
}

export interface QueuedMessagesProps {
  readonly items: readonly QueuedMessage[];
  readonly onSendNow: (id: string) => void;
  readonly onEdit: (id: string) => void;
  readonly onRemove: (id: string) => void;
  readonly testId?: string;
}

function traySx(theme: Theme) {
  return {
    display: 'flex',
    flexDirection: 'column',
    paddingX: 0.5,
    paddingY: 0.5,
    marginBottom: 0.5,
    borderRadius: theme.vars.shape.radiusMd,
    background: theme.vars.palette.background.userInputBackground,
  } as const;
}

function rowSx(theme: Theme) {
  return {
    display: 'flex',
    alignItems: 'center',
    gap: 0.25,
    minHeight: '1.75rem',
    paddingLeft: 1,
    borderRadius: theme.vars.shape.radiusSm,
    '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
  } as const;
}

function actionSx(theme: Theme) {
  return {
    width: '1.75rem',
    height: '1.75rem',
    padding: 0,
    borderRadius: theme.vars.shape.radiusSm,
    color: theme.vars.palette.text.metrics,
    '& svg': { width: '1rem', height: '1rem' },
    '&:hover': { background: theme.vars.palette.background.button.drawerMenu.selected },
  } as const;
}

function RowAction({ label, testId, onClick, children }: { readonly label: string; readonly testId: string; readonly onClick: () => void; readonly children: ReactNode }): ReactNode {
  return (
    <Tooltip title={label} placement="top" describeChild>
      <IconButton aria-label={label} data-testid={testId} onClick={onClick} sx={actionSx}>
        {children}
      </IconButton>
    </Tooltip>
  );
}

export function QueuedMessages({ items, onSendNow, onEdit, onRemove, testId = 'queued-messages' }: QueuedMessagesProps): ReactNode {
  if (items.length === 0) return null;
  const caption = t('shared.ui.queuedMessages.caption', 'Queued');
  return (
    <Box component="section" aria-label={caption} data-testid={testId} sx={traySx}>
      <Typography
        variant="labelSmall"
        component="p"
        sx={(theme: Theme) => ({ fontWeight: 600, lineHeight: '1.5rem', paddingX: 1, color: theme.vars.palette.text.metrics })}
      >
        {caption}
      </Typography>
      {items.map((item) => (
        <Box key={item.id} data-testid={item.testId ?? 'queued-message'} data-state={item.state} sx={rowSx}>
          <Typography variant="bodySmall" noWrap title={item.text} sx={(theme: Theme) => ({ flex: 1, minWidth: 0, color: theme.vars.palette.text.secondary })}>
            {item.text}
          </Typography>
          <RowAction label={t('shared.ui.queuedMessages.sendNow', 'Send now')} testId="queued-send-now" onClick={() => onSendNow(item.id)}>
            <ArrowUpwardIcon />
          </RowAction>
          <RowAction label={t('shared.ui.queuedMessages.edit', 'Edit')} testId="queued-edit" onClick={() => onEdit(item.id)}>
            <EditOutlinedIcon />
          </RowAction>
          <RowAction label={t('shared.ui.queuedMessages.remove', 'Remove')} testId="queued-remove" onClick={() => onRemove(item.id)}>
            <CloseIcon />
          </RowAction>
        </Box>
      ))}
    </Box>
  );
}
