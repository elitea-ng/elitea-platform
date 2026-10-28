import type { Theme } from '@mui/material/styles';

/**
 * The one reading column a conversation sits in — Chat and a Local work
 * thread alike: the transcript and the composer share it, centred, so a line
 * stays readable on a wide window and both screens take the same room. The
 * scroll container stays full width (its scrollbar at the window's edge);
 * the column goes inside it.
 */
export const CONVERSATION_COLUMN = {
  width: '100%',
  maxWidth: '48rem',
  marginX: 'auto',
  paddingX: 3,
  boxSizing: 'border-box',
} as const;

/**
 * A message bubble, the same on both screens: hugs a short message and takes
 * up to `maxWidth` of the column for a long one. `mine`: the reader's own
 * words, in the accent tint (the chat sets it on the right); anyone else's —
 * a person's, an agent's reply — in the neutral fill, on the left. No shadow,
 * no border.
 */
export function conversationBubbleSx(mine: boolean, maxWidth = '100%') {
  return (theme: Theme) =>
    ({
      width: 'fit-content',
      maxWidth,
      boxSizing: 'border-box',
      background: mine ? theme.vars.palette.background.select.selected.default : theme.vars.palette.background.userInputBackground,
      color: theme.vars.palette.text.secondary,
      borderRadius: theme.vars.shape.radiusLg,
      padding: '0.625rem 1rem',
      // Markdown's paragraph margins would pad the bubble's first and last line.
      '& p:first-of-type': { marginTop: 0 },
      '& p:last-child': { marginBottom: 0 },
    }) as const;
}
