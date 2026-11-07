/**
 * The browser control pill and the hand-back form (ADR-0048 decision 4):
 * one look and one meaning for the cloud pane (`CloudBrowserView`, #1598)
 * and the desktop pane (#1591).
 *
 * - The pill says who drives the page: **Agent**, **You**, **Idle** (no
 *   turn uses it) or **Ended** (a cloud run that is over).
 * - Hand back returns the page with an optional note: the agent reads it
 *   (a cloud run's tool result; on the desktop, the person's message, or
 *   the answer to the agent's `browser_request_user`).
 */
import { useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import TextField from '@mui/material/TextField';
import type { Theme } from '@mui/material/styles';

// The longest note, in UTF-8 bytes: the cloud view protocol's, and the desktop host's (`browser_control.rs`).
import { MAX_NOTE_BYTES } from '@/shared/api/browserView/protocol';
import { t } from '@/shared/i18n';

/** Who drives a browser page. */
export type BrowserPillState = 'agent' | 'user' | 'idle' | 'ended';

export function BrowserControlPill({ state, testId }: { readonly state: BrowserPillState; readonly testId: string }): React.JSX.Element {
  const label = {
    agent: t('cloudBrowser.pill.agent', 'Agent'),
    user: t('cloudBrowser.pill.you', 'You'),
    idle: t('cloudBrowser.pill.idle', 'Idle'),
    ended: t('cloudBrowser.pill.ended', 'Ended'),
  }[state];
  return <Chip size="small" label={label} color={state === 'user' ? 'warning' : 'default'} data-testid={testId} data-control={state} sx={{ flexShrink: 0 }} />;
}

interface HandBackFormProps {
  /** The trimmed note (empty when none). */
  readonly onSend: (note: string) => void;
  readonly onCancel: () => void;
  /** Test ids: `<prefix>-hand-back-form`, `-note`, `-hand-back-send`, `-hand-back-cancel`. */
  readonly testIdPrefix: string;
}

export function HandBackForm({ onSend, onCancel, testIdPrefix }: HandBackFormProps): React.JSX.Element {
  const [note, setNote] = useState('');
  const tooLong = new TextEncoder().encode(note).length > MAX_NOTE_BYTES;
  return (
    <Box
      component="form"
      data-testid={`${testIdPrefix}-hand-back-form`}
      onSubmit={(event) => {
        event.preventDefault();
        if (!tooLong) onSend(note.trim());
      }}
      sx={(theme: Theme) => ({ display: 'flex', gap: 1, alignItems: 'flex-start', paddingX: 1, paddingY: 0.75, borderBottom: `1px solid ${theme.vars.palette.divider}`, flexShrink: 0 })}
    >
      <TextField
        size="small"
        fullWidth
        multiline
        maxRows={4}
        value={note}
        onChange={(event) => setNote(event.target.value)}
        placeholder={t('cloudBrowser.handBack.placeholder', 'A note for the agent, for example "signed in, continue"')}
        error={tooLong}
        helperText={tooLong ? t('cloudBrowser.handBack.tooLong', 'The note is too long.') : undefined}
        slotProps={{ htmlInput: { 'aria-label': t('cloudBrowser.handBack.note', 'Note for the agent'), 'data-testid': `${testIdPrefix}-note` } }}
      />
      <Button type="submit" size="small" variant="contained" disabled={tooLong} data-testid={`${testIdPrefix}-hand-back-send`}>
        {t('cloudBrowser.handBack', 'Hand back')}
      </Button>
      <Button size="small" onClick={onCancel} data-testid={`${testIdPrefix}-hand-back-cancel`}>
        {t('cloudBrowser.cancel', 'Cancel')}
      </Button>
    </Box>
  );
}
